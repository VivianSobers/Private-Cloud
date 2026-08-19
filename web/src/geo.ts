// The map's arithmetic, kept out of the component so it can be tested without
// rendering anything.
//
// There is no map library here and no tile provider, which is the decision that
// held the map view up rather than the drawing. A map usually means tiles, and
// an offline-capable app whose whole premise is that your data stays yours
// cannot quietly send the coordinates of every photo you own to a stranger's
// server, one request per tile, every time somebody pans. So the map draws a
// graticule and plots the photos on it, and it works with the network off.

import type { Node } from "./api";

/** Web Mercator runs to infinity at the poles, so it is clamped where every
 *  other web map clamps it. */
export const MERCATOR_MAX_LAT = 85.05112878;

/** Projects a coordinate into view units: x is longitude, y is a Mercator
 *  latitude scaled to the same degrees-wide range and flipped so north is up.
 *  Both axes in the same units keeps the projection square, which is what makes
 *  shapes and bearings look the way people expect. */
export function project(lat: number, lon: number): { x: number; y: number } {
  const clamped = Math.max(-MERCATOR_MAX_LAT, Math.min(MERCATOR_MAX_LAT, lat));
  const radians = (clamped * Math.PI) / 180;
  const mercator = Math.log(Math.tan(Math.PI / 4 + radians / 2));
  return { x: lon, y: (-mercator * 180) / Math.PI };
}

/** The inverse, for labelling a horizontal grid line with its real latitude. */
export function unprojectLat(y: number): number {
  const mercator = (-y * Math.PI) / 180;
  return ((2 * Math.atan(Math.exp(mercator)) - Math.PI / 2) * 180) / Math.PI;
}

/** Grid spacing: the largest round step that still puts a few lines on screen.
 *  A fixed step is either a wall of lines when zoomed in or one line when zoomed
 *  out, and both are useless for telling where you are. */
export function gridStep(span: number): number {
  for (const step of [45, 15, 5, 1, 0.5, 0.1, 0.05, 0.01, 0.005, 0.001]) {
    if (span / step >= 3) return step;
  }
  return 0.001;
}

/** Longitudes wrap; a label of 190°E is wrong and 170°W is right. */
export function normaliseLon(lon: number): number {
  return ((((lon + 180) % 360) + 360) % 360) - 180;
}

export function formatLat(lat: number): string {
  return `${Math.abs(lat).toFixed(Math.abs(lat) < 1 ? 3 : 1)}°${lat >= 0 ? "N" : "S"}`;
}

export function formatLon(lon: number): string {
  const wrapped = normaliseLon(lon);
  return `${Math.abs(wrapped).toFixed(Math.abs(wrapped) < 1 ? 3 : 1)}°${wrapped >= 0 ? "E" : "W"}`;
}

/** A photo with a location, already projected. */
export interface Placed {
  node: Node;
  lat: number;
  lon: number;
  x: number;
  y: number;
}

/** A group of photos close enough together that separate pins would overlap. */
export interface Cluster {
  x: number;
  y: number;
  lat: number;
  lon: number;
  items: Placed[];
}

/** Groups pins that land within `radius` view units of each other.
 *
 *  Greedy and order-dependent, like the face clustering, and for once that is
 *  entirely harmless: this decides what a pin looks like, not what anything IS,
 *  and it is recomputed from scratch on every zoom. */
export function clusterPlaced(placed: Placed[], radius: number): Cluster[] {
  const clusters: Cluster[] = [];

  for (const p of placed) {
    const near = clusters.find(
      (c) => Math.abs(c.x - p.x) <= radius && Math.abs(c.y - p.y) <= radius,
    );
    if (!near) {
      clusters.push({ x: p.x, y: p.y, lat: p.lat, lon: p.lon, items: [p] });
      continue;
    }
    near.items.push(p);
    // Track the centre of what it holds, so a cluster sits among its photos
    // rather than on whichever one happened to arrive first.
    const n = near.items.length;
    near.x = near.items.reduce((sum, i) => sum + i.x, 0) / n;
    near.y = near.items.reduce((sum, i) => sum + i.y, 0) / n;
    near.lat = near.items.reduce((sum, i) => sum + i.lat, 0) / n;
    near.lon = near.items.reduce((sum, i) => sum + i.lon, 0) / n;
  }

  return clusters;
}

/** The view that shows every photo, with a margin. A single photo has no
 *  extent, so it gets a fixed span rather than a zoom of infinity — the case
 *  that turns a working map into a blank one. */
export function fitView(placed: Placed[]): { cx: number; cy: number; span: number } {
  if (placed.length === 0) return { cx: 0, cy: 0, span: 180 };

  const xs = placed.map((p) => p.x);
  const ys = placed.map((p) => p.y);
  const minX = Math.min(...xs);
  const maxX = Math.max(...xs);
  const minY = Math.min(...ys);
  const maxY = Math.max(...ys);

  return {
    cx: (minX + maxX) / 2,
    cy: (minY + maxY) / 2,
    span: Math.max((maxX - minX) / 2, (maxY - minY) / 2, 0.02) * 1.4,
  };
}
