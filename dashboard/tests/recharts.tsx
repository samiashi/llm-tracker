import { Children, isValidElement } from "react";
import type { ReactElement, ReactNode } from "react";

type Props = Record<string, unknown>;

/** One chart as Recharts was asked to draw it: its label, its rows and each series' props. */
export type Drawn = { label: string; data: Props[]; series: Props[] };

/**
 * Every chart drawn since the last reset. jsdom gives ResponsiveContainer no
 * size, so real Recharts draws nothing a test can inspect; the double below
 * records what each AreaChart was handed instead.
 */
export const drawn: Drawn[] = [];

/**
 * Recharts with its container and area chart replaced and everything else
 * real. The chart renders one element carrying its label, so a page test can
 * find a chart the way a screen reader would. Use it as
 * `vi.mock("recharts", async (real) => (await import("./recharts")).rechartsDouble(real))`.
 */
export async function rechartsDouble(real: () => Promise<typeof import("recharts")>) {
  const actual = await real();
  return {
    ...actual,
    ResponsiveContainer: ({ children }: { children: ReactNode }) => <>{children}</>,
    AreaChart: (props: { data: Props[]; children?: ReactNode; "aria-label"?: string }) => {
      const series = Children.toArray(props.children)
        .filter((c): c is ReactElement<Props> => isValidElement(c) && c.type === actual.Area)
        .map((c) => c.props);
      drawn.push({ label: props["aria-label"] ?? "", data: props.data, series });
      return <div role="img" aria-label={props["aria-label"]} />;
    },
  };
}
