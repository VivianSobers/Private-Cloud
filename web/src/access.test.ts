import { describe, expect, it } from "vitest";

import { effectiveOwner, effectiveRole, ownershipLabel, permissionsFor } from "./access";
import type { Node } from "./api";

// The ownership and permission decisions the shared-browsing views make.
//
// These are the rules that decide whether a Delete button is drawn at all, so
// they are worth asserting away from React: the failure mode is not an
// exception, it is a button that is offered and then refused with a 403 — or,
// worse, a file somebody else owns that looks exactly like your own.

function node(over: Partial<Node> = {}): Node {
  return {
    id: "n1",
    kind: "file",
    name: "budget.xlsx",
    path: "/Projects/budget.xlsx",
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-01T00:00:00Z",
    ...over,
  };
}

describe("effectiveRole", () => {
  it("is undefined for the caller's own file", () => {
    expect(effectiveRole(node())).toBeUndefined();
  });

  it("prefers the node's own access object", () => {
    expect(effectiveRole(node({ access: { role: "editor", owner: "alice", shared: true } }), "viewer")).toBe(
      "editor",
    );
  });

  it("falls back to the enclosing folder's grant", () => {
    // Inheritance is derived, not stored: a child of a granted folder carries no
    // access object of its own, and reading that as "mine" is the bug this
    // fallback exists to prevent.
    expect(effectiveRole(node(), "viewer")).toBe("viewer");
  });
});

describe("effectiveOwner", () => {
  it("names the owner from the node, then from the folder", () => {
    expect(effectiveOwner(node({ access: { role: "viewer", owner: "alice", shared: true } }))).toBe("alice");
    expect(effectiveOwner(node(), "bob")).toBe("bob");
    expect(effectiveOwner(node())).toBeUndefined();
  });
});

describe("permissionsFor", () => {
  it("gives the owner everything and marks nothing as shared", () => {
    const p = permissionsFor(node());
    expect(p).toMatchObject({ mine: true, canWrite: true, canShare: true, canMove: true });
    expect(p.role).toBeUndefined();
    expect(p.owner).toBeUndefined();
  });

  it("withholds every write from a viewer", () => {
    // A viewer grant is read-only, so the controls are not drawn rather than
    // drawn and answered with a 403.
    const p = permissionsFor(node({ access: { role: "viewer", owner: "alice", shared: true } }));
    expect(p).toMatchObject({ mine: false, role: "viewer", canWrite: false, canShare: false, canMove: false });
    expect(p.owner).toBe("alice");
  });

  it("lets an editor write but not re-share and not move", () => {
    // Only the owner may grant — an editor re-sharing spreads access beyond what
    // the owner can revoke. Move is owner-only too: both ends resolve against
    // the same owner, so moving a shared file into your own tree is refused.
    const p = permissionsFor(node({ access: { role: "editor", owner: "alice", shared: true } }));
    expect(p).toMatchObject({ mine: false, role: "editor", canWrite: true, canShare: false, canMove: false });
  });

  it("applies the enclosing folder's role to an unannotated child", () => {
    expect(permissionsFor(node(), "viewer", "alice")).toMatchObject({
      mine: false,
      canWrite: false,
      owner: "alice",
    });
    expect(permissionsFor(node(), "editor", "alice")).toMatchObject({ mine: false, canWrite: true });
  });

  it("treats an unannotated row on a server that ignores the opt-in as the caller's own", () => {
    // Graceful degradation: an older server cannot return anything but the
    // caller's own files, so an absent access object with no enclosing grant is
    // correctly read as ownership rather than as missing information.
    expect(permissionsFor(node())).toMatchObject({ mine: true, canWrite: true });
  });
});

describe("ownershipLabel", () => {
  it("is null for your own file, so no marker is drawn", () => {
    expect(ownershipLabel(node())).toBeNull();
  });

  it("names the owner on a shared file", () => {
    expect(ownershipLabel(node({ access: { role: "viewer", owner: "alice", shared: true } }))).toBe(
      "shared by alice",
    );
  });

  it("inherits the folder's owner for a child of a granted folder", () => {
    expect(ownershipLabel(node(), "editor", "alice")).toBe("shared by alice");
  });

  it("still says the file is not yours when the owner is unknown", () => {
    // Honest rather than silent: the role came from somewhere, so the row is
    // somebody else's even if this response did not say whose.
    expect(ownershipLabel(node(), "viewer")).toBe("shared with you");
  });
});
