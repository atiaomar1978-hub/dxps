import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, beforeEach, vi } from "vitest";
import { nav } from "./nav";

vi.mock("next/navigation", () => ({
  useParams: () => nav.params,
  useRouter: () => ({ push: nav.push, replace: nav.push, refresh: vi.fn(), back: vi.fn() }),
  usePathname: () => nav.pathname,
  useSearchParams: () => nav.search,
}));

beforeEach(() => {
  nav.reset();
  if (typeof window === "undefined") return;
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { pathname: "/", search: "", assign: vi.fn(), href: "http://localhost/" },
  });
});

afterEach(() => {
  if (typeof window !== "undefined") cleanup();
  vi.restoreAllMocks();
});
