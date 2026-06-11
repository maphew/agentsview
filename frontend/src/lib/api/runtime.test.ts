import { describe, expect, it } from "vite-plus/test";
import {
  ApiError,
  callGenerated,
} from "./runtime.js";
import {
  ApiError as GeneratedApiError,
} from "./generated/index";

describe("callGenerated", () => {
  it("normalizes generated API error bodies", async () => {
    await expect(
      callGenerated(async () => {
        throw new GeneratedApiError(
          { method: "GET", url: "/api/v1/usage/summary" },
          {
            url: "/api/v1/usage/summary",
            ok: false,
            status: 400,
            statusText: "Bad Request",
            body: { error: "invalid timezone: Fake/Zone" },
          },
          "Bad Request",
        );
      }),
    ).rejects.toMatchObject({
      name: "ApiError",
      status: 400,
      message: "invalid timezone: Fake/Zone",
    } satisfies Partial<ApiError>);
  });
});
