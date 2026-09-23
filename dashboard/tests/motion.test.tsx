import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { usePrefersReducedMotion } from "@/motion";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function Probe() {
  return <p>{usePrefersReducedMotion() ? "reduced" : "full"}</p>;
}

/** A matchMedia whose answer the test can change, as the system setting would. */
function systemSetting(initial: boolean) {
  let matches = initial;
  const listeners = new Set<() => void>();
  vi.stubGlobal("matchMedia", () => ({
    get matches() {
      return matches;
    },
    addEventListener: (_: string, l: () => void) => listeners.add(l),
    removeEventListener: (_: string, l: () => void) => listeners.delete(l),
  }));
  return (next: boolean) => {
    matches = next;
    for (const l of listeners) l();
  };
}

describe("usePrefersReducedMotion", () => {
  it("follows the system setting without a reload", () => {
    const set = systemSetting(false);
    render(<Probe />);
    expect(screen.getByText("full")).toBeTruthy();
    act(() => set(true));
    expect(screen.getByText("reduced")).toBeTruthy();
  });

  it("assumes full motion where matchMedia does not exist", () => {
    render(<Probe />);
    expect(screen.getByText("full")).toBeTruthy();
  });
});
