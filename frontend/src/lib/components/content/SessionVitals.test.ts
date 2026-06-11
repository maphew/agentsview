// @vitest-environment jsdom
import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from "vite-plus/test";
import { mount, tick, unmount } from "svelte";
import type { SessionTiming } from "../../api/types/timing.js";

const mocks = vi.hoisted(() => {
  const timing: SessionTiming = {
    session_id: "sess-1",
    total_duration_ms: 1200,
    tool_duration_ms: 0,
    turn_count: 1,
    tool_call_count: 0,
    subagent_count: 0,
    slowest_call: null,
    by_category: [],
    turns: [],
    running: false,
  };

  return {
    fetchSessionTiming: vi.fn().mockResolvedValue(timing),
  };
});

vi.mock("../../api/timing.js", () => ({
  fetchSessionTiming: mocks.fetchSessionTiming,
}));

import { ui } from "../../stores/ui.svelte.js";
import { sessionTiming } from "../../stores/sessionTiming.svelte.js";
// @ts-ignore
import SessionVitals from "./SessionVitals.svelte";

describe("SessionVitals", () => {
  let component: ReturnType<typeof mount> | undefined;

  beforeEach(() => {
    sessionTiming.reset();
    ui.vitalsOpen = true;
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    sessionTiming.reset();
    ui.vitalsOpen = false;
    document.body.innerHTML = "";
  });

  it("has an obvious close control inside the analysis pane", async () => {
    component = mount(SessionVitals, {
      target: document.body,
      props: { sessionId: "sess-1" },
    });
    await tick();
    await tick();

    const closeButton = document.querySelector<HTMLButtonElement>(
      'button[aria-label="Close session analysis"]',
    );

    expect(closeButton).not.toBeNull();
    expect(closeButton?.title).toBe("Close session analysis");

    closeButton!.click();
    await tick();

    expect(ui.vitalsOpen).toBe(false);
  });
});
