import { vi } from "vitest";

// nav is the state behind the next/navigation mock.
export const nav = {
  params: {} as Record<string, string>,
  pathname: "/",
  search: new URLSearchParams(),
  push: vi.fn(),
  reset() {
    this.params = {};
    this.pathname = "/";
    this.search = new URLSearchParams();
    this.push = vi.fn();
  },
};
