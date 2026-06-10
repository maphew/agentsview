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
// @ts-ignore
import SessionList from "./SessionList.svelte";
import sessionFilterControlSource from "../filters/SessionFilterControl.svelte?raw";
import { sessions } from "../../stores/sessions.svelte.js";
import { starred } from "../../stores/starred.svelte.js";

vi.mock("../../api/client.js", () => ({
  listSessions: vi.fn().mockResolvedValue({
    sessions: [],
    total: 0,
  }),
  getAgents: vi.fn().mockResolvedValue({ agents: [] }),
  getMachines: vi.fn().mockResolvedValue({ machines: [] }),
  getStats: vi.fn().mockResolvedValue({
    session_count: 0,
    message_count: 0,
    project_count: 0,
    machine_count: 0,
    earliest_session: null,
  }),
  watchEvents: vi.fn(() => ({ close: () => {} })),
}));

class ResizeObserverMock {
  observe = vi.fn();
  disconnect = vi.fn();
}

describe("SessionList filter dropdown", () => {
  let component: ReturnType<typeof mount> | undefined;
  let originalResizeObserver: typeof ResizeObserver | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: ResizeObserverMock,
    });
    sessions.sessions = [];
    sessions.agents = [];
    sessions.machines = [];
    sessions.activeSessionId = null;
    starred.filterOnly = false;
  });

  afterEach(() => {
    if (component) {
      unmount(component);
      component = undefined;
    }
    document.body.innerHTML = "";
    Object.defineProperty(globalThis, "ResizeObserver", {
      configurable: true,
      writable: true,
      value: originalResizeObserver,
    });
  });

  it("bounds the filter menu to the viewport and lets it scroll", async () => {
    component = mount(SessionList, { target: document.body });
    await tick();

    const filterButton = document.querySelector<HTMLButtonElement>(
      ".filter-btn",
    );
    expect(filterButton).not.toBeNull();

    filterButton!.click();
    await tick();

    const dropdown = document.querySelector<HTMLElement>(
      ".filter-dropdown",
    );
    expect(dropdown).not.toBeNull();

    expect(sessionFilterControlSource).toContain(
      "max-height: min(560px, calc(100vh - 128px));",
    );
    expect(sessionFilterControlSource).toContain("overflow-y: auto;");
  });
});
