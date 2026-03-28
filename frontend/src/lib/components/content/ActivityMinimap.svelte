<script lang="ts">
  import { sessionActivity } from "../../stores/sessionActivity.svelte.js";
  import { ui } from "../../stores/ui.svelte.js";
  import type { SessionActivityBucket } from "../../api/types/session-activity.js";

  const BAR_HEIGHT = 48;
  const BAR_GAP = 2;
  const MIN_BAR_WIDTH = 4;

  interface Props {
    sessionId: string;
  }

  let { sessionId }: Props = $props();

  let containerRef = $state<HTMLDivElement | null>(null);
  let chartRef = $state<HTMLDivElement | null>(null);
  let containerWidth = $state(300);

  $effect(() => {
    void sessionActivity.load(sessionId);
  });

  $effect(() => {
    if (!containerRef) return;
    const obs = new ResizeObserver((entries) => {
      for (const entry of entries) {
        containerWidth = entry.contentRect.width;
      }
    });
    obs.observe(containerRef);
    return () => obs.disconnect();
  });

  const chart = $derived.by(() => {
    const buckets = sessionActivity.buckets;
    if (buckets.length === 0) return null;

    const n = buckets.length;
    const barWidth = Math.max(
      MIN_BAR_WIDTH,
      Math.floor((containerWidth - n * BAR_GAP) / n),
    );

    let maxCount = 0;
    for (const b of buckets) {
      const total = b.user_count + b.assistant_count;
      if (total > maxCount) maxCount = total;
    }
    if (maxCount === 0) maxCount = 1;

    const bars = buckets.map((bucket, i) => {
      const total =
        bucket.user_count + bucket.assistant_count;
      const height = (total / maxCount) * BAR_HEIGHT;
      return {
        x: i * (barWidth + BAR_GAP),
        height,
        width: barWidth,
        populated: total > 0,
        bucket,
        index: i,
      };
    });

    const svgWidth =
      bars.length > 0
        ? bars[bars.length - 1]!.x + barWidth
        : containerWidth;

    return { bars, svgWidth };
  });

  let tooltip = $state<{
    x: number;
    y: number;
    text: string;
  } | null>(null);

  function handleBarHover(
    e: MouseEvent,
    bar: NonNullable<typeof chart>["bars"][number],
  ) {
    if (!bar.populated) return;
    const rect = (
      e.currentTarget as SVGElement
    ).getBoundingClientRect();
    const chartRect = chartRef?.getBoundingClientRect();
    if (!chartRect) return;
    const scrollLeft = chartRef?.scrollLeft ?? 0;
    const range =
      formatTime(bar.bucket.start_time) +
      "\u2013" +
      formatTime(bar.bucket.end_time);
    const total =
      bar.bucket.user_count + bar.bucket.assistant_count;
    tooltip = {
      x:
        rect.left +
        rect.width / 2 -
        chartRect.left +
        scrollLeft,
      y: rect.top - 4 - chartRect.top,
      text:
        `${range} \u2014 ${bar.bucket.user_count} user, ` +
        `${bar.bucket.assistant_count} assistant`,
    };
  }

  function handleBarLeave() {
    tooltip = null;
  }

  function handleBarClick(bucket: SessionActivityBucket) {
    if (bucket.first_ordinal == null) return;
    if (ui.hasBlockFilters) {
      ui.showAllBlocks();
    }
    ui.scrollToOrdinal(bucket.first_ordinal);
  }

  function handleBarKeydown(
    e: KeyboardEvent,
    bucket: SessionActivityBucket,
  ) {
    if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      handleBarClick(bucket);
    }
  }

  function formatTime(iso: string): string {
    const d = new Date(iso);
    return d.toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function handleRetry() {
    void sessionActivity.reload(sessionId);
  }

  const activeIndex = $derived(
    sessionActivity.activeBucketIndex,
  );

  const startTime = $derived(
    sessionActivity.buckets.length > 0
      ? formatTime(sessionActivity.buckets[0]!.start_time)
      : "",
  );

  const endTime = $derived(
    sessionActivity.buckets.length > 0
      ? formatTime(
          sessionActivity.buckets[
            sessionActivity.buckets.length - 1
          ]!.end_time,
        )
      : "",
  );
</script>

<div class="activity-minimap" bind:this={containerRef}>
  {#if sessionActivity.loading || !sessionActivity.loaded || !sessionActivity.isForSession(sessionId)}
    <div class="minimap-status">Loading activity...</div>
  {:else if sessionActivity.error}
    <div class="minimap-error">
      {sessionActivity.error}
      <button class="retry-btn" onclick={handleRetry}>
        Retry
      </button>
    </div>
  {:else if sessionActivity.buckets.length === 0}
    <div class="minimap-status">
      No timestamp data available
    </div>
  {:else if chart}
    <div class="minimap-chart" bind:this={chartRef}>
      <svg
        width={chart.svgWidth}
        height={BAR_HEIGHT}
        class="minimap-svg"
      >
        {#each chart.bars as bar}
          {#if bar.populated}
            <g
              class="minimap-bar minimap-bar--clickable"
              role="button"
              tabindex={0}
              aria-label="{formatTime(bar.bucket.start_time)}–{formatTime(bar.bucket.end_time)}: {bar.bucket.user_count} user, {bar.bucket.assistant_count} assistant"
              onclick={() => handleBarClick(bar.bucket)}
              onkeydown={(e) =>
                handleBarKeydown(e, bar.bucket)}
              onmouseenter={(e) => handleBarHover(e, bar)}
              onmouseleave={handleBarLeave}
            >
              <rect
                x={bar.x}
                y={BAR_HEIGHT - bar.height}
                width={bar.width}
                height={bar.height}
                rx="1"
                class="bar-fill"
              />
            </g>
          {:else}
            <rect
              x={bar.x}
              y={BAR_HEIGHT - 1}
              width={bar.width}
              height={1}
              class="bar-empty"
            />
          {/if}
          {#if activeIndex === bar.index}
            <rect
              x={bar.x - 1}
              y={bar.populated
                ? BAR_HEIGHT - bar.height - 1
                : BAR_HEIGHT - 2}
              width={bar.width + 2}
              height={bar.populated
                ? bar.height + 2
                : 3}
              rx="2"
              class="bar-indicator"
            />
          {/if}
        {/each}
      </svg>

      {#if tooltip}
        <div
          class="minimap-tooltip"
          style="left: {tooltip.x}px; top: {tooltip.y}px;"
        >
          {tooltip.text}
        </div>
      {/if}
    </div>

    <div class="minimap-axis">
      <span class="axis-time">{startTime}</span>
      <span class="axis-time">{endTime}</span>
    </div>
  {/if}
</div>

<style>
  .activity-minimap {
    padding: 6px 14px 4px;
    border-bottom: 1px solid var(--border-muted);
  }

  .minimap-status {
    color: var(--text-muted);
    font-size: 11px;
    padding: 4px 0;
    text-align: center;
  }

  .minimap-error {
    color: var(--accent-red);
    font-size: 11px;
    padding: 4px 0;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
  }

  .retry-btn {
    padding: 1px 6px;
    border: 1px solid currentColor;
    border-radius: var(--radius-sm);
    font-size: 10px;
    color: inherit;
    cursor: pointer;
    background: transparent;
  }

  .retry-btn:hover {
    background: var(--bg-surface-hover);
  }

  .minimap-chart {
    position: relative;
    overflow-x: auto;
  }

  .minimap-svg {
    display: block;
  }

  .bar-fill {
    fill: var(--accent-blue, #58a6ff);
  }

  .bar-empty {
    fill: var(--border-muted);
  }

  .bar-indicator {
    fill: none;
    stroke: var(--text-primary);
    stroke-width: 1.5;
    pointer-events: none;
  }

  .minimap-bar--clickable {
    cursor: pointer;
  }

  .minimap-bar--clickable:hover {
    opacity: 0.8;
  }

  .minimap-bar--clickable:focus-visible {
    outline: none;
  }

  .minimap-bar--clickable:focus-visible .bar-fill {
    opacity: 0.85;
  }

  .minimap-tooltip {
    position: absolute;
    transform: translateX(-50%) translateY(-100%);
    padding: 3px 8px;
    background: var(--text-primary);
    color: var(--bg-primary);
    font-size: 10px;
    border-radius: var(--radius-sm);
    white-space: nowrap;
    pointer-events: none;
    z-index: 100;
  }

  .minimap-axis {
    display: flex;
    justify-content: space-between;
    align-items: center;
    font-size: 9px;
    color: var(--text-muted);
    padding-top: 2px;
    height: 16px;
    user-select: none;
  }

  .axis-time {
    flex-shrink: 0;
  }
</style>
