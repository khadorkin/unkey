import { getTenantId } from "@/lib/auth";
import { db, eq, schema } from "@/lib/db";
import { redirect } from "next/navigation";
import { Onboarding } from "./client";

export default async function OnboardingPage() {
  const tenantId = getTenantId();
  const workspace = await db.query.workspaces.findFirst({
    where: eq(schema.workspaces.tenantId, tenantId),
  });

  if (workspace) {
    return redirect("/app/apis");
  }

  return <Onboarding tenantId={tenantId} />;
}
