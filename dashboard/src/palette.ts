/**
 * Categorical slots, assigned by position and never cycled: a filter that
 * changes how many series are drawn must not repaint the survivors.
 */
export const SLOTS = [
  "var(--series-1)",
  "var(--series-2)",
  "var(--series-3)",
  "var(--series-4)",
  "var(--series-5)",
  "var(--series-6)",
  "var(--series-7)",
  "var(--series-8)",
  "var(--series-9)",
];

/** Everything past the last slot, as one neutral segment: it is no category, so it gets no hue. */
export const OTHER_FILL = "var(--muted-fill)";

/**
 * The model chart's slots. The server is asked for exactly this many models,
 * so the chart never runs out; past six, unstacked lines stop being readable.
 */
export const MODEL_SLOTS = SLOTS.slice(0, 6);

/** One colour per token kind, shared by every card that draws them. */
export const TOKEN_KIND = {
  input: SLOTS[0],
  output: SLOTS[1],
  cacheRead: SLOTS[2],
  cacheWrite: SLOTS[3],
};
