// Feedback on machine output: the pure half.
//
// The server records a person's judgement on a chat answer, a citation, a
// "similar" neighbour, a search hit or a face, and a `wrong` verdict suppresses
// that result for that person in later answers of the same kind. Nothing is
// retrained — the effect is in the server's query — so from here it is an
// ordinary write plus one piece of state worth caching: which verdicts already
// stand, so a control renders as already-pressed instead of forgetting what the
// user told it a moment ago.
//
// Everything that decides *what* to render lives in this file with no JSX in
// sight, so it can be tested without a DOM. Feedback.tsx is the thin part.

import { api, ApiError, type Feedback, type FeedbackKind, type FeedbackVerdict } from "./api";

/** What a control is attached to. Mirrors the submission shape: `answer` is
 *  identified by the question in `context`, `face` by `person_id`, and the rest
 *  by `node_id`. */
export interface FeedbackTarget {
  kind: FeedbackKind;
  node_id?: string;
  person_id?: string;
  context?: string;
}

/** The buttons, in the order they are shown. Three, not a star rating: "not
 *  helpful" and "wrong" are different claims and only one of them changes what
 *  the server serves back, which is worth separating in the UI as well as in the
 *  schema. */
export const VERDICTS: ReadonlyArray<{
  value: FeedbackVerdict;
  label: string;
  title: string;
}> = [
  { value: "helpful", label: "👍", title: "Helpful" },
  { value: "not_helpful", label: "👎", title: "Not helpful for what I asked" },
  { value: "wrong", label: "✕", title: "Wrong — stop showing me this one" },
];

/** A stable key for one target, so a standing verdict can be looked up without
 *  comparing objects.
 *
 *  The separator is a pipe, which cannot occur in a kind or a uuid — the three
 *  fields before it. `context` is free text and may well contain one, but it is
 *  last, so every separator that delimits anything sits between fields that
 *  cannot contain it. A separator a field CAN contain is how two different
 *  targets come to share a key. */
export function targetKey(t: FeedbackTarget): string {
  return [t.kind, t.node_id ?? "", t.person_id ?? "", (t.context ?? "").trim()].join("|");
}

/** Index the server's list by target, so a control knows what it already said.
 *
 *  Walked in reverse so the FIRST-listed entry wins. The server returns newest
 *  first and keeps only one standing verdict per target, so this decides nothing
 *  today; it matters only if that ever stops being true, and "the newest verdict
 *  is the one that counts" is the answer that would still be right. */
export function indexVerdicts(list: Feedback[]): Record<string, FeedbackVerdict> {
  const out: Record<string, FeedbackVerdict> = {};
  for (let i = list.length - 1; i >= 0; i--) {
    const f = list[i];
    if (f) out[targetKey(f)] = f.verdict;
  }
  return out;
}

/** Whether a failure means "this server has no feedback endpoint".
 *
 *  Only ever asked of `GET /feedback`, and that is the whole trick. A 404 from a
 *  submission is ambiguous — the route may be missing, or the node may be one the
 *  caller cannot read, which the server deliberately answers identically. A read
 *  of your own feedback names no target at all, so nothing about it can be
 *  missing except the route itself. */
export function isRouteAbsent(e: unknown): boolean {
  return e instanceof ApiError && e.status === 404;
}

/** What the controls know once the probe has answered. */
export interface FeedbackState {
  /** False on a server too old to have the endpoint — the controls render as
   *  absent rather than broken, the way every other panel in this app degrades. */
  supported: boolean;
  verdicts: Record<string, FeedbackVerdict>;
}

let pending: Promise<FeedbackState> | null = null;

/** Load the caller's standing verdicts, once per session.
 *
 *  Cached as the PROMISE, not the result: six controls mounting in the same tick
 *  would otherwise each fire the probe, and the answer is identical for all of
 *  them.
 *
 *  A failure that is not a missing route — offline, a 500 — resolves to
 *  "supported, nothing known" rather than "absent". Hiding the controls because
 *  the network blinked would remove a working feature for the rest of the
 *  session, and the cache means it would not come back on its own. */
export function loadFeedback(): Promise<FeedbackState> {
  pending ??= api
    .feedback()
    .then((r) => ({ supported: true, verdicts: indexVerdicts(r.feedback) }))
    .catch((e: unknown) =>
      isRouteAbsent(e)
        ? { supported: false, verdicts: {} }
        : { supported: true, verdicts: {} },
    );
  return pending;
}

/** Drop the cached probe. Only tests and a sign-out have any use for this. */
export function resetFeedback(): void {
  pending = null;
}

/** Submit one verdict and return the verdict now standing for that target.
 *
 *  Returns null when the server refused, which the caller renders by leaving the
 *  control as it was: a button that lights up for a write that did not happen is
 *  worse than one that does nothing, because it tells somebody their correction
 *  has been taken when it has not. */
export async function submitVerdict(
  target: FeedbackTarget,
  verdict: FeedbackVerdict,
  note?: string,
): Promise<FeedbackVerdict | null> {
  try {
    await api.submitFeedback({ ...target, verdict, note });
  } catch {
    return null;
  }
  const state = await loadFeedback();
  // Update the cache in place rather than re-reading the list. The server has
  // just told us it stored this, and a second round trip per click would make a
  // one-word opinion cost two requests.
  state.verdicts[targetKey(target)] = verdict;
  return verdict;
}
