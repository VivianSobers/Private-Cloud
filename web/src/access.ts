// What the caller may do with a node, and whose it is.
//
// Split out of the views deliberately. These are authorisation *decisions*, and
// the same three of them (a row in the browser, a search hit, a file under a
// tag) were being re-derived inline in three places from slightly different
// expressions — which is exactly how a Move button ends up offered in one list
// and withheld in another for the same file. One function, one answer, and the
// answer is testable without rendering React.
//
// None of this is a security boundary: the server refuses what it refuses. It
// exists so the UI does not OFFER an action it knows will come back 403, and so
// a file somebody else owns never looks like your own.

import type { Node, Role } from "./api";

/** What the UI may offer for one node, and how to describe who owns it. */
export interface Permissions {
  /** The caller's own file. `access` absent and no inherited grant. */
  mine: boolean;
  /** The caller's role, or undefined when the node is their own. */
  role?: Role;
  /** Rename, delete, upload into, tag. An editor's writes land in the owner's
   *  tree and on the owner's quota; a viewer's are refused. */
  canWrite: boolean;
  /** Only the owner may grant. An editor re-sharing would spread access beyond
   *  what the owner can see or revoke, so the button is not offered at all. */
  canShare: boolean;
  /** Owner-only even for an editor: both ends of a move resolve against the
   *  same owner, so moving a shared file into your own tree is refused
   *  server-side rather than silently copying it onto your quota. */
  canMove: boolean;
  /** The owner's username, when it is not the caller. */
  owner?: string;
}

/** The role that governs a node.
 *
 *  A node inside a granted folder carries no `access` of its own — inheritance
 *  is derived, not stored, and only the rows the server annotated get one — so
 *  the enclosing folder's role stands in. Undefined means the caller owns it,
 *  which is also what an older server that ignores `?include_shared=true`
 *  produces for every row: it can only ever return the caller's own files, so
 *  "no access object" reads correctly as "mine" on both servers.
 */
export function effectiveRole(node: Node, inherited?: Role): Role | undefined {
  return node.access?.role ?? inherited;
}

/** The owner's username when the node is somebody else's, undefined when it is
 *  the caller's own. The enclosing folder's owner stands in for a child, for the
 *  same inheritance reason as the role. */
export function effectiveOwner(node: Node, inheritedOwner?: string): string | undefined {
  return node.access?.owner ?? inheritedOwner;
}

/** Every rendering decision for one node, from the fields the API actually
 *  returns: `access.role`, `access.owner`, `access.shared`. */
export function permissionsFor(node: Node, inherited?: Role, inheritedOwner?: string): Permissions {
  const role = effectiveRole(node, inherited);
  const mine = role === undefined || role === "owner";
  return {
    mine,
    role,
    canWrite: mine || role === "editor",
    canShare: mine,
    canMove: mine,
    owner: mine ? undefined : effectiveOwner(node, inheritedOwner),
  };
}

/** A short, honest ownership marker for a listing row: "shared by alice", or
 *  null for the caller's own file. Null rather than "" so a caller cannot
 *  accidentally render an empty separator beside it. */
export function ownershipLabel(node: Node, inherited?: Role, inheritedOwner?: string): string | null {
  const p = permissionsFor(node, inherited, inheritedOwner);
  if (p.mine) return null;
  return p.owner ? `shared by ${p.owner}` : "shared with you";
}
