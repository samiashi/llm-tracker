import { useState } from "react";
import type { InputHTMLAttributes } from "react";
import { isISODate } from "@/window";

/**
 * A date box that changes the range only with a whole date the range can
 * take. A browser reports every keystroke of a typed year as a date -- 0002,
 * 0020, 0202, then 2025 -- and committing each clamps the first to the
 * window's floor and writes it back, resetting the year being typed. Until a
 * date is whole and in range it is a draft of this box's own, dropped when
 * the box is left.
 */
export function DateBox({
  value,
  lo,
  hi,
  onCommit,
  ...input
}: {
  value: string;
  /** The first and last day the range can take; `min` and `max` may be narrower. */
  lo: string;
  hi: string;
  onCommit: (day: string) => void;
} & Omit<InputHTMLAttributes<HTMLInputElement>, "type" | "value" | "onChange" | "onBlur">) {
  const [draft, setDraft] = useState<string | null>(null);
  return (
    <input
      {...input}
      type="date"
      value={draft ?? value}
      onChange={(e) => {
        const day = e.target.value;
        if (isISODate(day) && day >= lo && day <= hi) {
          setDraft(null);
          onCommit(day);
        } else {
          setDraft(day);
        }
      }}
      onBlur={() => setDraft(null)}
    />
  );
}
