import { Suspense, type ReactNode } from "react";
import { Shell } from "@/components/Shell";

export default function MainLayout({ children }: { children: ReactNode }) {
  return (
    <Shell>
      <Suspense fallback={<p className="muted">Loading…</p>}>{children}</Suspense>
    </Shell>
  );
}
