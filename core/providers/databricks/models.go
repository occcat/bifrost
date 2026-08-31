// Model discovery. Neither Databricks surface exposes an OpenAI-shaped /v1/models route, so
// each is listed through its own workspace REST API and normalised here.
package databricks

import (
	"net/http"
	"strings"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/valyala/fasthttp"
)

// DatabricksServingEndpointsResponse is the GET /api/2.0/serving-endpoints response. Only the
// fields Bifrost surfaces are declared; the rest of each endpoint object is ignored.
type DatabricksServingEndpointsResponse struct {
	Endpoints []struct {
		Name  string `json:"name"`
		ID    string `json:"id,omitempty"`
		Task  string `json:"task,omitempty"`
		State *struct {
			Ready string `json:"ready,omitempty"`
		} `json:"state,omitempty"`
	} `json:"endpoints"`
}

// DatabricksModelServicesResponse is the GET /api/2.1/unity-catalog/model-services response.
// A model service is addressed by its fully qualified Unity Catalog name.
type DatabricksModelServicesResponse struct {
	ModelServices []struct {
		Name        string `json:"name"`
		CatalogName string `json:"catalog_name,omitempty"`
		SchemaName  string `json:"schema_name,omitempty"`
		FullName    string `json:"full_name,omitempty"`
		Comment     string `json:"comment,omitempty"`
	} `json:"model_services"`
}

// modelIDs returns the wire model names a serving-endpoints listing exposes.
func (r *DatabricksServingEndpointsResponse) modelIDs() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.Endpoints))
	for _, e := range r.Endpoints {
		if e.Name == "" {
			continue
		}
		// A not-yet-ready endpoint would fail every inference request; skip it rather than
		// advertising a model that cannot serve. An endpoint with no state reported is kept.
		if e.State != nil && e.State.Ready != "" && !strings.EqualFold(e.State.Ready, "READY") {
			continue
		}
		ids = append(ids, e.Name)
	}
	return ids
}

// modelIDs returns the wire model names a Unity Catalog model-services listing exposes.
// Model services are addressed by their fully qualified <catalog>.<schema>.<name>.
func (r *DatabricksModelServicesResponse) modelIDs() []string {
	if r == nil {
		return nil
	}
	ids := make([]string, 0, len(r.ModelServices))
	for _, s := range r.ModelServices {
		if s.FullName != "" {
			ids = append(ids, s.FullName)
			continue
		}
		if s.CatalogName != "" && s.SchemaName != "" && s.Name != "" {
			ids = append(ids, s.CatalogName+"."+s.SchemaName+"."+s.Name)
			continue
		}
		if s.Name != "" {
			ids = append(ids, s.Name)
		}
	}
	return ids
}

// toBifrostListModelsResponse applies the shared allowlist/blacklist/alias pipeline to a set
// of upstream model names.
func (provider *DatabricksProvider) toBifrostListModelsResponse(modelIDs []string, key schemas.Key, unfiltered bool) *schemas.BifrostListModelsResponse {
	response := &schemas.BifrostListModelsResponse{Data: make([]schemas.Model, 0, len(modelIDs))}

	pipeline := &providerUtils.ListModelsPipeline{
		AllowedModels:     key.Models,
		BlacklistedModels: key.BlacklistedModels,
		Aliases:           key.Aliases,
		Unfiltered:        unfiltered,
		ProviderKey:       provider.GetProviderKey(),
		MatchFns:          providerUtils.DefaultMatchFns(),
	}
	if pipeline.ShouldEarlyExit() {
		return response
	}

	included := make(map[string]bool)
	for _, id := range modelIDs {
		for _, result := range pipeline.FilterModel(id) {
			entry := schemas.Model{
				ID:   string(provider.GetProviderKey()) + "/" + result.ResolvedID,
				Name: schemas.Ptr(id),
			}
			if result.AliasValue != "" {
				entry.Alias = schemas.Ptr(result.AliasValue)
			}
			response.Data = append(response.Data, entry)
			included[strings.ToLower(result.ResolvedID)] = true
		}
	}
	response.Data = append(response.Data, pipeline.BackfillModels(included)...)
	return response
}

// listModelsByKey lists the models reachable with one key, from whichever surface that key
// targets. In auto mode the model name is what selects a surface, and there is no model to
// inspect when listing, so auto lists Model Serving — the surface that serves every
// Databricks-hosted foundation model.
func (provider *DatabricksProvider) listModelsByKey(ctx *schemas.BifrostContext, key schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	listPath := modelServingListPath
	if key.DatabricksKeyConfig != nil && key.DatabricksKeyConfig.APIFormat == schemas.DatabricksAPIFormatAIGateway {
		listPath = aiGatewayListPath
	}

	url, bErr := provider.workspaceAPIURL(key, listPath)
	if bErr != nil {
		return nil, bErr
	}
	// A context path override may point at either listing, or at an entirely different
	// workspace API, so honour it ahead of the resolved default.
	if override := providerUtils.GetPathFromContext(ctx, ""); override != "" {
		if strings.HasPrefix(override, "http://") || strings.HasPrefix(override, "https://") {
			url = override
		} else {
			url, bErr = provider.workspaceAPIURL(key, override)
			if bErr != nil {
				return nil, bErr
			}
		}
	}

	auth, bErr := provider.authHeader(key)
	if bErr != nil {
		return nil, bErr
	}

	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	providerUtils.SetExtraHeaders(ctx, req, provider.networkConfig.ExtraHeaders, nil)
	req.SetRequestURI(url)
	req.Header.SetMethod(http.MethodGet)
	req.Header.SetContentType("application/json")
	for k, v := range auth {
		req.Header.Set(k, v)
	}

	latency, bifrostErr, wait := providerUtils.MakeRequestWithContext(ctx, provider.client, req, resp)
	defer wait()
	if bifrostErr != nil {
		return nil, bifrostErr
	}

	ctx.SetValue(schemas.BifrostContextKeyProviderResponseHeaders, providerUtils.ExtractProviderResponseHeaders(resp))

	if resp.StatusCode() != fasthttp.StatusOK {
		return nil, providerUtils.SetErrorLatency(parseDatabricksError(resp), latency)
	}

	body, err := providerUtils.CheckAndDecodeBody(resp)
	if err != nil {
		return nil, providerUtils.NewBifrostOperationError(schemas.ErrProviderResponseDecode, err)
	}

	var modelIDs []string
	var rawRequest, rawResponse interface{}
	sendRawRequest := providerUtils.ShouldSendBackRawRequest(ctx, provider.sendBackRawRequest)
	sendRawResponse := providerUtils.ShouldSendBackRawResponse(ctx, provider.sendBackRawResponse)

	if listPath == aiGatewayListPath {
		var parsed DatabricksModelServicesResponse
		rawRequest, rawResponse, bifrostErr = providerUtils.HandleProviderResponse(body, &parsed, nil, sendRawRequest, sendRawResponse)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		modelIDs = parsed.modelIDs()
	} else {
		var parsed DatabricksServingEndpointsResponse
		rawRequest, rawResponse, bifrostErr = providerUtils.HandleProviderResponse(body, &parsed, nil, sendRawRequest, sendRawResponse)
		if bifrostErr != nil {
			return nil, bifrostErr
		}
		modelIDs = parsed.modelIDs()
	}

	response := provider.toBifrostListModelsResponse(modelIDs, key, request.Unfiltered)
	response.ExtraFields.Latency = latency.Milliseconds()
	if sendRawRequest {
		response.ExtraFields.RawRequest = rawRequest
	}
	if sendRawResponse {
		response.ExtraFields.RawResponse = rawResponse
	}
	return response, nil
}

// ListModels lists the Databricks models reachable with the configured keys.
func (provider *DatabricksProvider) ListModels(ctx *schemas.BifrostContext, keys []schemas.Key, request *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return providerUtils.HandleMultipleListModelsRequests(ctx, keys, request, provider.listModelsByKey)
}
