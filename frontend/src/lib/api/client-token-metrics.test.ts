import {
  describe,
  it,
  expect,
  vi,
  beforeEach,
  afterEach,
} from "vite-plus/test";
import {
  getAnalyticsHeatmap,
  getAnalyticsTopSessions,
} from "./client.js";

describe("analytics token metric query serialization", () => {
  let fetchSpy: ReturnType<typeof vi.fn>;
  let localStorageGetItem: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    fetchSpy = vi.fn().mockResolvedValue({
      ok: true,
      json: () => Promise.resolve({}),
    });
    vi.stubGlobal("fetch", fetchSpy);
    localStorageGetItem = vi.fn().mockReturnValue(null);
    vi.stubGlobal("localStorage", {
      getItem: localStorageGetItem,
      setItem: vi.fn(),
      removeItem: vi.fn(),
      clear: vi.fn(),
    });
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it("serializes output_tokens for heatmap", async () => {
    await getAnalyticsHeatmap({
      from: "2024-01-01",
      metric: "output_tokens",
    });

    expect(fetchSpy).toHaveBeenCalledWith(
      "/api/v1/analytics/heatmap?from=2024-01-01&metric=output_tokens",
      expect.any(Object),
    );
  });

  it("serializes output_tokens for top sessions", async () => {
    await getAnalyticsTopSessions({
      from: "2024-01-01",
      metric: "output_tokens",
    });

    expect(fetchSpy).toHaveBeenCalledWith(
      "/api/v1/analytics/top-sessions?from=2024-01-01&metric=output_tokens",
      expect.any(Object),
    );
  });
});
