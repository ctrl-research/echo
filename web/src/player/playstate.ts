/**
 * Playstate reporting.
 *
 * This is what makes Jellyfin count a play, show this client in its dashboard,
 * and remember where a track was left. It is also the only source of play
 * history: there is no separate history API, so what is not reported here never
 * happened as far as PlayCount and LastPlayedDate are concerned.
 */

import { clientFor } from "../jellyfin/client";
import type { Session } from "../jellyfin/session";

/** Jellyfin measures time in ticks of 100 nanoseconds. */
const TICKS_PER_SECOND = 10_000_000;

export const toTicks = (seconds: number): number =>
  Math.round(seconds * TICKS_PER_SECOND);

export const fromTicks = (ticks: number): number => ticks / TICKS_PER_SECOND;

export async function reportStart(
  session: Session,
  itemId: string,
): Promise<void> {
  await clientFor(session).POST("/Sessions/Playing", {
    body: { ItemId: itemId, PositionTicks: 0, IsPaused: false },
  });
}

export async function reportProgress(
  session: Session,
  itemId: string,
  positionSeconds: number,
  paused: boolean,
): Promise<void> {
  await clientFor(session).POST("/Sessions/Playing/Progress", {
    body: {
      ItemId: itemId,
      PositionTicks: toTicks(positionSeconds),
      IsPaused: paused,
    },
  });
}

export async function reportStopped(
  session: Session,
  itemId: string,
  positionSeconds: number,
): Promise<void> {
  await clientFor(session).POST("/Sessions/Playing/Stopped", {
    body: { ItemId: itemId, PositionTicks: toTicks(positionSeconds) },
  });
}
