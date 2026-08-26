import { useEffect, useState } from "react";

import type { FeedbackVerdict } from "./api";
import { loadFeedback, submitVerdict, targetKey, VERDICTS, type FeedbackTarget } from "./feedback";

// The controls themselves. Everything that decides what to show lives in
// feedback.ts; this file is the three buttons and the state they hold.
//
// GRACEFUL DEGRADATION, the pattern this repository uses everywhere: an older
// server has no /feedback route, `loadFeedback` sees the 404 on a request that
// names no target, and this renders NOTHING. Not a disabled button, not an error
// — a rating widget that is visibly broken is worse than no rating widget, and
// the cost of degrading quietly is named in status.md's retrospective and
// accepted there.

export function FeedbackControls({
  target,
  label,
}: {
  target: FeedbackTarget;
  /** Screen-reader name for the group, e.g. "feedback on this answer". The
   *  buttons themselves are emoji, which say nothing to anyone not looking. */
  label: string;
}) {
  const [supported, setSupported] = useState(false);
  const [verdict, setVerdict] = useState<FeedbackVerdict | undefined>();
  const [busy, setBusy] = useState(false);

  const key = targetKey(target);

  useEffect(() => {
    let live = true;
    void loadFeedback().then((s) => {
      if (!live) return;
      setSupported(s.supported);
      setVerdict(s.verdicts[key]);
    });
    return () => {
      live = false;
    };
  }, [key]);

  if (!supported) return null;

  const click = async (v: FeedbackVerdict) => {
    setBusy(true);
    // A null result means the write failed, and the control stays exactly as it
    // was — lighting it up anyway would tell somebody their correction had been
    // taken when it had not.
    const stored = await submitVerdict(target, v);
    if (stored) setVerdict(stored);
    setBusy(false);
  };

  return (
    <span className="feedback" role="group" aria-label={label}>
      {VERDICTS.map((v) => (
        <button
          key={v.value}
          type="button"
          className={`feedback-btn${verdict === v.value ? " chosen" : ""}`}
          title={v.title}
          aria-label={v.title}
          aria-pressed={verdict === v.value}
          disabled={busy}
          onClick={(e) => {
            // These sit inside rows and overlays that navigate on click; without
            // this, rating a search hit also opens it.
            e.stopPropagation();
            void click(v.value);
          }}
        >
          {v.label}
        </button>
      ))}
      {/* Said once, next to the only button that has an effect, rather than in a
          tooltip nobody opens: "wrong" is not a rating, it changes what this
          server will show this person next time. */}
      {verdict === "wrong" && <span className="muted small">hidden from your results</span>}
    </span>
  );
}
