import { beforeEach, describe, expect, it, vi } from "vite-plus/test";
import { trends } from "./trends.svelte.js";
import * as api from "../api/client.js";
import type { TrendsTermsResponse } from "../api/types.js";

vi.mock("../api/client.js", () => ({
  getTrendsTerms: vi.fn(),
}));

function makeResponse(): TrendsTermsResponse {
  return {
    granularity: "week",
    from: "2024-01-01",
    to: "2024-01-31",
    message_count: 0,
    buckets: [],
    series: [],
  };
}

function resetStore() {
  trends.from = "2024-01-01";
  trends.to = "2024-01-31";
  trends.granularity = "week";
  trends.normalized = false;
  trends.termText = "load bearing | load-bearing\nseam";
  trends.response = null;
  trends.loading.terms = false;
  trends.errors.terms = null;
}

beforeEach(() => {
  resetStore();
  vi.clearAllMocks();
  vi.mocked(api.getTrendsTerms).mockResolvedValue(makeResponse());
});

describe("TrendsStore.fetchTerms", () => {
  it("fetches default terms with timezone and date range", async () => {
    await trends.fetchTerms();

    expect(api.getTrendsTerms).toHaveBeenCalledWith(
      expect.objectContaining({
        from: "2024-01-01",
        to: "2024-01-31",
        granularity: "week",
        terms: ["load bearing | load-bearing", "seam"],
        timezone: expect.any(String),
      }),
    );
    expect(trends.response?.granularity).toBe("week");
  });

  it("removes blank term lines", async () => {
    trends.termText = "seam\n\n  \nblast radius";

    await trends.fetchTerms();

    expect(api.getTrendsTerms).toHaveBeenCalledWith(
      expect.objectContaining({
        terms: ["seam", "blast radius"],
      }),
    );
  });

  it("sets first-load error state", async () => {
    vi.mocked(api.getTrendsTerms).mockRejectedValue(new Error("boom"));

    await trends.fetchTerms();

    expect(trends.response).toBeNull();
    expect(trends.loading.terms).toBe(false);
    expect(trends.errors.terms).toBe("boom");
  });

  it("keeps existing response and surfaces refetch errors", async () => {
    const existing = makeResponse();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    trends.response = existing;
    vi.mocked(api.getTrendsTerms).mockRejectedValue(new Error("boom"));

    await trends.fetchTerms();

    expect(trends.response).toEqual(existing);
    expect(trends.loading.terms).toBe(false);
    expect(trends.errors.terms).toBe("boom");
    expect(warn).toHaveBeenCalledWith(
      "trends.terms refetch failed:",
      expect.any(Error),
    );
    warn.mockRestore();
  });

  it("setGranularity refetches with the new granularity", async () => {
    await trends.setGranularity("month");

    expect(api.getTrendsTerms).toHaveBeenCalledWith(
      expect.objectContaining({ granularity: "month" }),
    );
  });
});
