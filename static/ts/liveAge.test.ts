// liveAge.test.ts — bun test
//
// Pins the TypeScript humaniser against the same boundary
// table as the Go TestHumanizeAgeBoundaries. The two
// implementations MUST agree or the age column visibly jumps
// on the first tick; a divergence that's only caught by a
// user reporting "the number keeps flickering" is exactly
// what these tests prevent.
//
// Run with: bun test static/ts/liveAge.test.ts

import { describe, expect, test } from "bun:test";

import { humanizeAge } from "./liveAge";

describe("humanizeAge — boundary parity with Go side", () => {
  const cases: Array<[string, number, string]> = [
    ["zero", 0, "0s"],
    ["sub-second", 500, "0s"],
    ["negative-duration", -5000, "0s"],
    ["exactly-one-second", 1000, "1s"],
    ["42s", 42_000, "42s"],
    ["59s", 59_000, "59s"],
    ["minute-boundary", 60_000, "1m"],
    ["3m", 3 * 60_000, "3m"],
    ["59m", 59 * 60_000, "59m"],
    ["hour-boundary", 60 * 60_000, "1h"],
    ["23h", 23 * 60 * 60_000, "23h"],
    ["day-boundary", 24 * 60 * 60_000, "1d"],
    ["12d", 12 * 24 * 60 * 60_000, "12d"],
    ["364d", 364 * 24 * 60 * 60_000, "364d"],
    ["year-boundary", 365 * 24 * 60 * 60_000, "1y"],
    ["3y", 3 * 365 * 24 * 60 * 60_000, "3y"],
  ];

  for (const [name, deltaMs, want] of cases) {
    test(name, () => {
      expect(humanizeAge(deltaMs)).toBe(want);
    });
  }
});
