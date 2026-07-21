// WebAuthn ceremony plumbing.
//
// The browser API speaks ArrayBuffer; the server speaks base64url JSON. All the
// conversion lives here so no component ever has to think about it — and so the
// encoding is done one way, since a base64 (rather than base64url) slip
// produces a challenge mismatch that fails with no useful message.

import { ApiError } from "./api";

function b64urlToBuffer(value: string): ArrayBuffer {
  const padded = value.replace(/-/g, "+").replace(/_/g, "/");
  const binary = atob(padded.padEnd(padded.length + ((4 - (padded.length % 4)) % 4), "="));
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
  return bytes.buffer;
}

function bufferToB64url(buf: ArrayBuffer): string {
  const bytes = new Uint8Array(buf);
  let binary = "";
  for (let i = 0; i < bytes.length; i++) binary += String.fromCharCode(bytes[i]!);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Browsers without WebAuthn cannot use this server at all — there is no password. */
export function isSupported(): boolean {
  return typeof window !== "undefined" && !!window.PublicKeyCredential;
}

/**
 * hasPlatformAuthenticator reports whether the device has a built-in
 * authenticator (Touch ID, Windows Hello, Android screen lock). Used only to
 * word the prompt honestly — a security key still works when this is false.
 */
export async function hasPlatformAuthenticator(): Promise<boolean> {
  if (!isSupported()) return false;
  try {
    return await PublicKeyCredential.isUserVerifyingPlatformAuthenticatorAvailable();
  } catch {
    return false;
  }
}

interface CredentialCreationJSON {
  publicKey: {
    challenge: string;
    rp: PublicKeyCredentialRpEntity;
    user: { id: string; name: string; displayName: string };
    pubKeyCredParams: PublicKeyCredentialParameters[];
    timeout?: number;
    excludeCredentials?: { id: string; type: "public-key"; transports?: string[] }[];
    authenticatorSelection?: AuthenticatorSelectionCriteria;
    attestation?: AttestationConveyancePreference;
  };
}

interface CredentialRequestJSON {
  publicKey: {
    challenge: string;
    timeout?: number;
    rpId?: string;
    allowCredentials?: { id: string; type: "public-key"; transports?: string[] }[];
    userVerification?: UserVerificationRequirement;
  };
}

async function post<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  const parsed = text ? JSON.parse(text) : undefined;
  if (!res.ok) {
    const e = parsed?.error;
    throw new ApiError(res.status, e?.code ?? "unknown", e?.message ?? "request failed", e?.request_id);
  }
  return parsed as T;
}

/**
 * register enrols a passkey.
 *
 * `username` is only meaningful during bootstrap — when an account already
 * exists, the server takes the identity from the session and ignores it.
 */
export async function register(username: string, credentialName: string): Promise<unknown> {
  const options = await post<CredentialCreationJSON>("/api/v1/auth/register/begin", { username });
  const pk = options.publicKey;

  const created = (await navigator.credentials.create({
    publicKey: {
      ...pk,
      challenge: b64urlToBuffer(pk.challenge),
      user: {
        ...pk.user,
        id: b64urlToBuffer(pk.user.id),
      },
      excludeCredentials: pk.excludeCredentials?.map((c) => ({
        ...c,
        id: b64urlToBuffer(c.id),
        transports: c.transports as AuthenticatorTransport[] | undefined,
      })),
    },
  })) as PublicKeyCredential | null;

  if (!created) throw new Error("the browser returned no credential");
  const response = created.response as AuthenticatorAttestationResponse;

  return post(
    `/api/v1/auth/register/finish?name=${encodeURIComponent(credentialName)}`,
    {
      id: created.id,
      rawId: bufferToB64url(created.rawId),
      type: created.type,
      // Preserved so the server can record whether the passkey is synced to a
      // cloud keychain — the difference between "losing this laptop loses the
      // key" and "it is already on the phone".
      clientExtensionResults: created.getClientExtensionResults(),
      response: {
        clientDataJSON: bufferToB64url(response.clientDataJSON),
        attestationObject: bufferToB64url(response.attestationObject),
        transports: response.getTransports?.() ?? [],
      },
    },
  );
}

/** login runs an assertion ceremony and establishes a session cookie. */
export async function login(username: string): Promise<unknown> {
  const options = await post<CredentialRequestJSON>("/api/v1/auth/login/begin", { username });
  const pk = options.publicKey;

  const assertion = (await navigator.credentials.get({
    publicKey: {
      ...pk,
      challenge: b64urlToBuffer(pk.challenge),
      allowCredentials: pk.allowCredentials?.map((c) => ({
        ...c,
        id: b64urlToBuffer(c.id),
        transports: c.transports as AuthenticatorTransport[] | undefined,
      })),
    },
  })) as PublicKeyCredential | null;

  if (!assertion) throw new Error("the browser returned no credential");
  const response = assertion.response as AuthenticatorAssertionResponse;

  return post("/api/v1/auth/login/finish", {
    id: assertion.id,
    rawId: bufferToB64url(assertion.rawId),
    type: assertion.type,
    clientExtensionResults: assertion.getClientExtensionResults(),
    response: {
      clientDataJSON: bufferToB64url(response.clientDataJSON),
      authenticatorData: bufferToB64url(response.authenticatorData),
      signature: bufferToB64url(response.signature),
      userHandle: response.userHandle ? bufferToB64url(response.userHandle) : "",
    },
  });
}

/**
 * describeError turns a WebAuthn exception into something a person can act on.
 * The raw DOMExceptions are notoriously unhelpful — "NotAllowedError" covers
 * both "you cancelled" and "the origin is wrong", which need opposite responses.
 */
export function describeError(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof DOMException) {
    switch (err.name) {
      case "NotAllowedError":
        return "The prompt was dismissed, timed out, or this site's address does not match the one the passkey was created for.";
      case "InvalidStateError":
        return "That authenticator already holds a passkey for this account.";
      case "NotSupportedError":
        return "This device cannot create the kind of passkey the server asked for.";
      case "SecurityError":
        return "The page origin is not valid for WebAuthn. It must be HTTPS (or localhost) and match PC_WEBAUTHN_RPID.";
      case "AbortError":
        return "Cancelled.";
    }
    return err.message || err.name;
  }
  return err instanceof Error ? err.message : String(err);
}
