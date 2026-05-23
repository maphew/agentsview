import { test, expect } from "@playwright/test";
import { SessionsPage } from "./pages/sessions-page";
import {
  waitForStableValue,
  waitForRowCountStable,
} from "./helpers/virtual-list-helpers";

test.describe("Message loading", () => {
  test("clicking session shows messages", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();
  });

  test("no request spam on session click", async ({ page }) => {
    const messageRequests: string[] = [];
    page.on("request", (req) => {
      if (req.url().includes("/messages")) {
        messageRequests.push(req.url());
      }
    });

    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    // Wait for at least one message request to have fired
    await expect
      .poll(() => messageRequests.length, { timeout: 5_000 })
      .toBeGreaterThan(0);

    // Wait for requests to stop firing
    await waitForStableValue(() => messageRequests.length, 500);

    // For large sessions we may fetch several pages while loading
    // into memory. With the reactive loop bug, this would be
    // dozens of parallel requests.
    expect(messageRequests.length).toBeLessThanOrEqual(15);
  });

  test("small session loads fast", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectLastSession();
  });

  test(
    "large session shows first page quickly",
    async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();

      // First session is the largest (5500 messages)
      await sp.sessionItems.first().click();

      // First page should render within 3s
      await expect(sp.messageRows.first()).toBeVisible({
        timeout: 3_000,
      });
    },
  );

  test(
    "scroll does not reset to top during loading",
    async ({ page }) => {
      const sp = new SessionsPage(page);
      await sp.goto();
      await sp.selectFirstSession();

      // Wait for progressive loading to finish by polling
      // the message row count until it stabilizes.
      await waitForRowCountStable(sp);

      // Scroll down
      await sp.scroller.evaluate((el) => {
        el.scrollTop = 3000;
      });

      // Wait for scroll position to settle
      await expect
        .poll(
          () => sp.scroller.evaluate((el) => el.scrollTop),
          { timeout: 2_000 },
        )
        .toBeGreaterThan(500);
    },
  );

  test("follow latest scrolls down and exits on manual wheel", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await expect(follow).toHaveAttribute("aria-pressed", "false");

    await sp.scroller.evaluate((el) => {
      el.scrollTop = el.scrollHeight;
      el.dispatchEvent(new Event("scroll"));
    });
    await expect
      .poll(
        () =>
          sp.scroller.evaluate(
            (el) =>
              el.scrollHeight - el.clientHeight - el.scrollTop,
          ),
        { timeout: 2_000 },
      )
      .toBeLessThanOrEqual(8);

    await follow.click();

    await expect
      .poll(
        () =>
          sp.scroller.evaluate(
            (el) =>
              el.scrollHeight - el.clientHeight - el.scrollTop,
          ),
        { timeout: 2_000 },
      )
      .toBeLessThanOrEqual(8);
    await expect(follow).toHaveAttribute("aria-pressed", "true");

    await sp.scroller.hover();
    await page.mouse.wheel(0, -1800);

    await expect(follow).toHaveAttribute("aria-pressed", "false");
  });

  test("follow latest settles after a tall final message is measured", async ({
    page,
  }) => {
    await page.route(
      "**/api/v1/sessions/test-session-xlarge-5500/messages*",
      async (route) => {
        const now = new Date().toISOString();
        const messages = Array.from(
          { length: 1000 },
          (_, i) => {
            const ordinal = 4500 + i;
            const isLast = ordinal === 5499;
            const content = isLast
              ? Array.from(
                  { length: 120 },
                  (_, n) =>
                    `Final response paragraph ${n}. This line makes the final message much taller than the virtualizer estimate.`,
                ).join("\n\n")
              : `Message ${ordinal}`;
            return {
              id: ordinal,
              session_id: "test-session-xlarge-5500",
              ordinal,
              role: ordinal % 2 === 0 ? "user" : "assistant",
              content,
              timestamp: now,
              has_thinking: false,
              thinking_text: "",
              has_tool_use: false,
              content_length: content.length,
              model: "",
              token_usage: null,
              context_tokens: 0,
              output_tokens: 0,
              has_context_tokens: false,
              has_output_tokens: false,
              tool_calls: [],
              is_system: false,
            };
          },
        );

        await route.fulfill({
          json: {
            messages: [...messages].reverse(),
            count: messages.length,
          },
        });
      },
    );

    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    await page.getByLabel("Follow latest messages").click();

    await expect
      .poll(
        () =>
          sp.scroller.evaluate(
            (el) =>
              el.scrollHeight - el.clientHeight - el.scrollTop,
          ),
        { timeout: 3_000 },
      )
      .toBeLessThanOrEqual(8);
  });

  test("follow latest stays enabled through non-user scroll drift", async ({
    page,
  }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "true");

    await page.waitForTimeout(1100);
    await sp.scroller.evaluate((el) => {
      el.scrollTop = 0;
      el.dispatchEvent(new Event("scroll"));
    });

    await expect(follow).toHaveAttribute("aria-pressed", "true");
  });

  test("follow latest button toggles off", async ({ page }) => {
    const sp = new SessionsPage(page);
    await sp.goto();
    await sp.selectFirstSession();

    const follow = page.getByLabel("Follow latest messages");
    await expect(follow).toHaveAttribute("aria-pressed", "false");
    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "true");

    await follow.click();
    await expect(follow).toHaveAttribute("aria-pressed", "false");
  });
});
