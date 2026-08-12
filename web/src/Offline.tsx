import { useState } from "react";

import { api, formatBytes } from "./api";
import { listPinned, supportsPinning, unpinFile, type PinnedFile } from "./pin";

// Files pinned for offline access. Their bytes live in the browser's cache and
// the service worker serves them when the network is gone, so a pinned file opens
// on a plane or a dead spot. Pin a file from the photo viewer's "Pin offline".

export function Offline() {
  const [files, setFiles] = useState<PinnedFile[]>(listPinned());

  if (!supportsPinning())
    return <p className="muted">Offline files aren't supported in this browser.</p>;

  const unpin = async (id: string) => {
    await unpinFile(id);
    setFiles(listPinned());
  };

  return (
    <section className="stack">
      <p className="muted small">
        Files kept on this device for offline access. A pinned file opens even with
        no connection.
      </p>
      {files.length === 0 ? (
        <p className="muted">
          Nothing pinned yet. Open a photo and choose “Pin offline” to keep it here.
        </p>
      ) : (
        <ul className="shared-list">
          {files.map((f) => (
            <li key={f.id} className="row shared-item">
              <span className="shared-icon" aria-hidden="true">
                📄
              </span>
              <span className="stack" style={{ flex: 1, gap: 0 }}>
                <a className="link" href={api.contentUrl(f.id)}>
                  {f.name}
                </a>
                <span className="muted small">
                  {f.path}
                  {f.size != null ? ` · ${formatBytes(f.size)}` : ""}
                </span>
              </span>
              <button className="link danger" onClick={() => void unpin(f.id)}>
                Unpin
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
