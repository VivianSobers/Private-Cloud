import { describe, expect, it } from "vitest";

import type { Node } from "./api";
import {
  clusterPlaced,
  fitView,
  formatLat,
  formatLon,
  gridStep,
  normaliseLon,
  project,
  unprojectLat,
  type Placed,
} from "./geo";

function node(id: string): Node {
  return {
    id,
    kind: "file",
    name: `${id}.jpg`,
    path: `/${id}.jpg`,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

function at(id: string, lat: number, lon: number): Placed {
  return { node: node(id), lat, lon, ...project(lat, lon) };
}

describe("projection", () => {
  it("puts the equator and prime meridian at the origin", () => {
    // toBeCloseTo rather than toEqual: the y here is a negated zero, and strict
    // equality separates -0 from 0 for a difference nothing can see.
    expect(project(0, 0).x).toBeCloseTo(0, 12);
    expect(project(0, 0).y).toBeCloseTo(0, 12);
  });

  it("puts north above south", () => {
    // The sign is the bug that renders every map upside down, so it is asserted
    // rather than assumed: SVG y grows downward.
    expect(project(45, 0).y).toBeLessThan(project(-45, 0).y);
  });

  it("clamps the poles instead of returning infinity", () => {
    expect(Number.isFinite(project(90, 0).y)).toBe(true);
    expect(Number.isFinite(project(-90, 0).y)).toBe(true);
  });

  it("round-trips a latitude through the inverse", () => {
    for (const lat of [-80, -33.9, 0, 12.5, 51.5, 84]) {
      expect(unprojectLat(project(lat, 0).y)).toBeCloseTo(lat, 6);
    }
  });
});

describe("labels", () => {
  it("wraps a longitude past the antimeridian rather than printing 190°E", () => {
    expect(normaliseLon(190)).toBeCloseTo(-170);
    expect(formatLon(190)).toBe("170.0°W");
  });

  it("names the hemispheres", () => {
    expect(formatLat(51.5)).toBe("51.5°N");
    expect(formatLat(-33.9)).toBe("33.9°S");
    expect(formatLon(139.7)).toBe("139.7°E");
    expect(formatLon(-0.1)).toBe("0.100°W");
  });
});

describe("gridStep", () => {
  it("keeps at least a few lines on screen at every zoom", () => {
    for (const span of [360, 90, 10, 2, 0.4, 0.05, 0.004]) {
      const step = gridStep(span);
      expect(span / step).toBeGreaterThanOrEqual(3);
    }
  });

  it("uses coarser lines for a wider view", () => {
    expect(gridStep(360)).toBeGreaterThan(gridStep(10));
  });
});

describe("clustering", () => {
  it("groups pins that would overlap and keeps distant ones apart", () => {
    const clusters = clusterPlaced(
      [at("a", 51.5, -0.12), at("b", 51.5001, -0.1201), at("c", 35.7, 139.7)],
      0.01,
    );
    expect(clusters).toHaveLength(2);
    expect(clusters[0]?.items).toHaveLength(2);
    expect(clusters[1]?.items).toHaveLength(1);
  });

  it("sits a cluster among its photos, not on the first one", () => {
    const [cluster] = clusterPlaced([at("a", 10, 10), at("b", 10.2, 10.2)], 1);
    expect(cluster?.lat).toBeCloseTo(10.1, 6);
    expect(cluster?.lon).toBeCloseTo(10.1, 6);
  });

  it("returns nothing for nothing", () => {
    expect(clusterPlaced([], 1)).toEqual([]);
  });
});

describe("fitView", () => {
  it("centres on the photos and leaves a margin", () => {
    const view = fitView([at("a", 0, -10), at("b", 0, 10)]);
    expect(view.cx).toBeCloseTo(0, 6);
    // 10 degrees of half-extent, plus the margin.
    expect(view.span).toBeGreaterThan(10);
  });

  it("gives a single photo a usable span rather than a zoom of infinity", () => {
    const view = fitView([at("a", 51.5, -0.12)]);
    expect(view.span).toBeGreaterThan(0);
    expect(Number.isFinite(view.span)).toBe(true);
  });

  it("shows the whole world when there is nothing to fit", () => {
    expect(fitView([])).toEqual({ cx: 0, cy: 0, span: 180 });
  });
});
