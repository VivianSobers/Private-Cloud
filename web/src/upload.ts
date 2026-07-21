// Upload strategy.
//
// Small files go through the plain POST endpoint: one request, no session row,
// no staging file. Large files go through tus, because a 4 GB transfer over a
// phone connection will fail eventually and starting over from zero is the
// difference between a usable file server and a toy.
//
// The threshold is the only interesting decision here. Too low and every
// trivial upload pays for a session round-trip; too high and a failure late in
// a large transfer costs everything.

import * as tus from "tus-js-client";

/** Files at or above this size get resumable uploads. */
export const RESUMABLE_THRESHOLD = 8 * 1024 * 1024;

export interface UploadHandle {
  /** abort stops the transfer. Resumable uploads keep their progress server-side. */
  abort: () => void;
}

export interface UploadCallbacks {
  onProgress: (sent: number, total: number) => void;
  onDone: () => void;
  onError: (message: string) => void;
}

export function upload(
  file: File,
  parentId: string,
  cb: UploadCallbacks,
): UploadHandle {
  return file.size >= RESUMABLE_THRESHOLD
    ? resumableUpload(file, parentId, cb)
    : simpleUpload(file, parentId, cb);
}

/**
 * simpleUpload uses XMLHttpRequest rather than fetch.
 *
 * fetch still cannot report upload progress in any shipping browser, and an
 * upload with no progress bar is indistinguishable from a hung one.
 */
function simpleUpload(file: File, parentId: string, cb: UploadCallbacks): UploadHandle {
  const xhr = new XMLHttpRequest();
  const url = `/api/v1/upload?parent_id=${encodeURIComponent(parentId)}&name=${encodeURIComponent(file.name)}`;

  xhr.open("POST", url);
  xhr.withCredentials = true;
  xhr.setRequestHeader("Content-Type", file.type || "application/octet-stream");

  xhr.upload.addEventListener("progress", (e) => {
    if (e.lengthComputable) cb.onProgress(e.loaded, e.total);
  });
  xhr.addEventListener("load", () => {
    if (xhr.status >= 200 && xhr.status < 300) {
      cb.onDone();
      return;
    }
    cb.onError(errorMessage(xhr.responseText, xhr.status));
  });
  xhr.addEventListener("error", () => cb.onError("the connection failed"));
  xhr.addEventListener("abort", () => cb.onError("cancelled"));
  xhr.send(file);

  return { abort: () => xhr.abort() };
}

function resumableUpload(file: File, parentId: string, cb: UploadCallbacks): UploadHandle {
  const up = new tus.Upload(file, {
    endpoint: "/api/v1/uploads",
    // No withCredentials: the endpoint is same-origin (Caddy fronts both the
    // app and the API), and XHR sends cookies on same-origin requests
    // automatically. tus-js-client v4 dropped the option entirely — enabling
    // credentials there now means reaching into onBeforeRequest, which would
    // be pure ceremony for a request that already carries the session.
    //
    // 5 MiB chunks: small enough that a dropped connection loses little, large
    // enough that per-request overhead stays negligible.
    chunkSize: 5 * 1024 * 1024,
    retryDelays: [0, 1000, 3000, 5000, 10000],
    metadata: {
      filename: file.name,
      filetype: file.type || "application/octet-stream",
      parent_id: parentId,
    },
    // Persist the upload URL so closing the tab and returning resumes rather
    // than restarting — the entire point of using tus.
    storeFingerprintForResuming: true,
    removeFingerprintOnSuccess: true,
    onProgress: (sent, total) => cb.onProgress(sent, total),
    onSuccess: () => cb.onDone(),
    onError: (err) => cb.onError(tusErrorMessage(err)),
  });

  // Resume from a previous session if this exact file was interrupted before.
  void up.findPreviousUploads().then((previous) => {
    if (previous.length > 0 && previous[0]) up.resumeFromPreviousUpload(previous[0]);
    up.start();
  });

  return { abort: () => void up.abort(true) };
}

function errorMessage(body: string, status: number): string {
  try {
    const parsed = JSON.parse(body) as { error?: { message?: string } };
    if (parsed.error?.message) return parsed.error.message;
  } catch {
    /* fall through to the status */
  }
  if (status === 507) return "storage quota exceeded";
  if (status === 413) return "the file is too large for this endpoint";
  if (status === 409) return "something with that name is already here";
  return `upload failed (${status})`;
}

function tusErrorMessage(err: Error | tus.DetailedError): string {
  const detailed = err as tus.DetailedError;
  const res = detailed.originalResponse;
  if (res) return errorMessage(res.getBody() ?? "", res.getStatus());
  return err.message || "upload failed";
}
