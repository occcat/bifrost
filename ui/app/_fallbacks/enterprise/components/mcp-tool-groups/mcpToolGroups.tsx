import PageTitle from "@/components/pageTitle";
import { ToolCase } from "lucide-react";
import { useTranslation } from "react-i18next";
import ContactUsView from "../views/contactUsView";

export default function MCPToolGroups() {
	const { t } = useTranslation("mcp");

	return (
		<>
			{/* The name and description live in the topbar, like every other page —
			    an inline <h2> here would just repeat the title shown above it. */}
			<PageTitle title={t("toolGroups.title")}>{t("toolGroups.description")}</PageTitle>
			<div className="rounded-sm border">
				<div className="flex w-full flex-col items-center justify-center py-16">
					<ContactUsView
						className="mx-auto w-full max-w-lg"
						icon={<ToolCase className="h-[5.5rem] w-[5.5rem]" strokeWidth={1} />}
						title={t("toolGroups.unlockTitle")}
						description={t("toolGroups.unlockDescription")}
						readmeLink="https://docs.getbifrost.ai/mcp/overview"
					/>
				</div>
			</div>
		</>
	);
}
