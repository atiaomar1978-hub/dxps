import { proxy } from "@/lib/proxy";
import { upstream } from "@/lib/upstream";

export const dynamic = "force-dynamic";

type Ctx = { params: Promise<{ path: string[] }> };

async function handler(req: Request, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  return proxy(req, path, upstream());
}

export { handler as GET, handler as POST, handler as PUT, handler as DELETE };
