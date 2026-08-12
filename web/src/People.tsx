import { useCallback, useEffect, useState } from "react";

import { api, ApiError, type Node, type Person } from "./api";

// People: face clusters the server found. Each is a group of photos it believes
// are the same person; a user names a cluster, and can open it to see its photos.
// Reads the Phase 8 /people surface.

export function People() {
  const [open, setOpen] = useState<Person | null>(null);
  return open ? (
    <PersonDetail person={open} onBack={() => setOpen(null)} />
  ) : (
    <PeopleGrid onOpen={setOpen} />
  );
}

function PeopleGrid({ onOpen }: { onOpen: (p: Person) => void }) {
  const [people, setPeople] = useState<Person[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api
      .people()
      .then((r) => live && setPeople(r.people))
      .catch((e) => live && setError(describe(e)));
    return () => {
      live = false;
    };
  }, []);

  if (error) return <Unavailable detail={error} />;
  if (!people) return <p className="muted">Loading people…</p>;
  if (people.length === 0)
    return (
      <p className="muted">
        No people found yet. As photos are analysed, faces get grouped here for you
        to name.
      </p>
    );

  return (
    <div className="album-grid">
      {people.map((p) => (
        <button key={p.id} className="album-tile" onClick={() => onOpen(p)} title={p.name ?? "Unnamed"}>
          <div className="album-cover">
            {p.cover_node_id ? (
              <Thumb id={p.cover_node_id} alt={p.name ?? "person"} />
            ) : (
              <span className="album-cover-empty" aria-hidden="true" />
            )}
          </div>
          <div className="album-meta">
            <span className="album-name">{p.name ?? <span className="muted">Unnamed</span>}</span>
            <span className="muted small">
              {p.face_count} {p.face_count === 1 ? "photo" : "photos"}
            </span>
          </div>
        </button>
      ))}
    </div>
  );
}

function PersonDetail({ person, onBack }: { person: Person; onBack: () => void }) {
  const [items, setItems] = useState<Node[] | null>(null);
  const [name, setName] = useState(person.name ?? "");
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(() => {
    api
      .person(person.id)
      .then((r) => {
        setItems(r.items);
        setName(r.person.name ?? "");
      })
      .catch((e) => setError(describe(e)));
  }, [person.id]);
  useEffect(load, [load]);

  const rename = useCallback(async () => {
    const next = window.prompt("Name this person", name)?.trim();
    if (next == null) return;
    try {
      await api.namePerson(person.id, next);
      setName(next);
    } catch (e) {
      setError(describe(e));
    }
  }, [person.id, name]);

  return (
    <div className="stack">
      <div className="row small">
        <button className="link" onClick={onBack}>
          ← People
        </button>
        <strong>{name || "Unnamed"}</strong>
        <button className="link" onClick={() => void rename()}>
          {name ? "Rename" : "Name this person"}
        </button>
      </div>
      {error && <div className="banner error small">{error}</div>}
      {!items ? (
        <p className="muted">Loading…</p>
      ) : items.length === 0 ? (
        <p className="muted">No photos in this group.</p>
      ) : (
        <div className="photo-grid">
          {items.map((n) => (
            <a
              key={n.id}
              className="photo-tile"
              href={api.downloadUrl(n.id)}
              title={n.name}
            >
              <Thumb id={n.id} alt={n.name} />
            </a>
          ))}
        </div>
      )}
    </div>
  );
}

function Thumb({ id, alt }: { id: string; alt: string }) {
  const [broken, setBroken] = useState(false);
  if (broken) return <span className="thumb-fallback">{alt}</span>;
  return (
    <img
      className="thumb"
      src={api.contentUrl(id, "thumb")}
      alt={alt}
      loading="lazy"
      onError={() => setBroken(true)}
    />
  );
}

function Unavailable({ detail }: { detail: string }) {
  return (
    <div className="muted">
      <p>People isn't available on this server yet.</p>
      <p className="small">{detail}</p>
    </div>
  );
}

function describe(e: unknown): string {
  if (e instanceof ApiError) {
    if (e.status === 404) return "The /people endpoints are not deployed on this server yet.";
    return `${e.code}: ${e.message}`;
  }
  return e instanceof Error ? e.message : "Unknown error";
}
