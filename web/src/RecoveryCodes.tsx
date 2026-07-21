import { useState } from "react";

interface Props {
  codes: string[];
  onDismiss: () => void;
}

/**
 * RecoveryCodes shows a freshly issued set.
 *
 * Dismissal is deliberately gated behind an explicit "I have saved these"
 * checkbox. The codes are stored only as argon2id hashes and cannot be shown
 * again — a stray click on a close button is not something the user can undo.
 */
export function RecoveryCodes({ codes, onDismiss }: Props) {
  const [confirmed, setConfirmed] = useState(false);
  const [copied, setCopied] = useState(false);

  if (codes.length === 0) return null;
  const text = codes.join("\n");

  return (
    <div className="banner warn stack">
      <strong>Recovery codes — shown once</strong>
      <p className="small" style={{ margin: 0 }}>
        Each code works once and gets you a 15-minute session that can do
        nothing but enrol a new passkey. Print them and keep them with the ZFS
        passphrase. They are stored only as hashes, so this is the one and only
        time they can be displayed.
      </p>

      <pre className="codes">{text}</pre>

      <div className="row">
        <button
          onClick={async () => {
            try {
              await navigator.clipboard.writeText(text);
              setCopied(true);
            } catch {
              // Clipboard access is blocked outside a secure context or without
              // permission. The codes are on screen regardless.
              setCopied(false);
            }
          }}
        >
          {copied ? "Copied" : "Copy"}
        </button>
        <button onClick={() => window.print()}>Print</button>
        <label className="row small">
          <input type="checkbox" checked={confirmed} onChange={(e) => setConfirmed(e.target.checked)} />
          I have saved these somewhere safe
        </label>
        <button className="primary" disabled={!confirmed} onClick={onDismiss}>
          Done
        </button>
      </div>
    </div>
  );
}
