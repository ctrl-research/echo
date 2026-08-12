import { create } from "zustand";
import type { components } from "../api/schema";

export type Track = components["schemas"]["TrackDTO"];

export type RepeatMode = "off" | "all" | "one";

type PlayerState = {
  queue: Track[];
  /**
   * Which list the queue came from — an album id, a playlist id, "search", and
   * so on. Needed because a track id is not enough to identify a row: a
   * playlist may hold the same song twice, and highlighting by id lights up
   * every copy instead of the one actually playing.
   */
  queueId: string;
  index: number;
  playing: boolean;
  /** Seconds. Mirrored from the audio element so the UI can render a scrubber. */
  position: number;
  duration: number;
  volume: number;
  repeat: RepeatMode;
  shuffle: boolean;

  /**
   * A permutation of queue indices, and how far through it we are.
   *
   * Shuffle is an order, not a dice roll per track. Picking at random each time
   * plays the same song twice in a row often enough to be irritating, and can
   * play one track three times before others have played at all. Walking a
   * permutation guarantees every track plays once before any plays twice, which
   * makes a consecutive repeat impossible except where the listener asked for
   * one.
   */
  shuffleOrder: number[];
  shufflePos: number;

  playQueue: (tracks: Track[], startAt?: number, queueId?: string) => void;
  toggle: () => void;
  next: () => void;
  previous: () => void;
  setRepeat: (mode: RepeatMode) => void;
  toggleShuffle: () => void;
  setVolume: (v: number) => void;

  /** Written by the audio element, not by the UI. */
  _sync: (patch: Partial<Pick<PlayerState, "playing" | "position" | "duration">>) => void;
};

export const current = (s: PlayerState): Track | null => s.queue[s.index] ?? null;

/** Fisher-Yates. Returns a new array; does not mutate the input. */
function shuffled(indices: number[]): number[] {
  const out = indices.slice();
  for (let i = out.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [out[i], out[j]] = [out[j], out[i]];
  }
  return out;
}

/**
 * Builds a shuffle order over `length` items.
 *
 * When `startAt` is a valid index the order begins there, so enabling shuffle
 * never interrupts what is playing. Pass -1 for no anchor, and `avoid` to keep
 * a particular index out of first place — used when reshuffling at the end of a
 * cycle so the track that just finished does not immediately open the next one.
 */
export function buildShuffleOrder(
  length: number,
  startAt: number,
  avoid?: number,
): number[] {
  if (length <= 0) return [];
  if (length === 1) return [0];

  if (startAt >= 0 && startAt < length) {
    const rest = shuffled(
      Array.from({ length }, (_, i) => i).filter((i) => i !== startAt),
    );
    return [startAt, ...rest];
  }

  const order = shuffled(Array.from({ length }, (_, i) => i));
  if (avoid !== undefined && order.length > 1 && order[0] === avoid) {
    [order[0], order[1]] = [order[1], order[0]];
  }
  return order;
}

export const usePlayer = create<PlayerState>((set, get) => ({
  queue: [],
  queueId: "",
  index: 0,
  playing: false,
  position: 0,
  duration: 0,
  volume: 1,
  repeat: "off",
  shuffle: false,
  shuffleOrder: [],
  shufflePos: 0,

  playQueue: (tracks, startAt = 0, queueId = "") => {
    const { shuffle } = get();
    set({
      queue: tracks,
      queueId,
      index: startAt,
      playing: true,
      position: 0,
      duration: 0,
      // A new queue invalidates any existing order.
      shuffleOrder: shuffle ? buildShuffleOrder(tracks.length, startAt) : [],
      shufflePos: 0,
    });
  },

  toggle: () => set((s) => ({ playing: !s.playing })),

  next: () => {
    const { queue, index, repeat, shuffle, shuffleOrder, shufflePos } = get();
    if (queue.length === 0) return;

    // Repeat-one restarts the same track rather than advancing. This is the one
    // case where hearing a track twice in a row is exactly what was asked for.
    if (repeat === "one") {
      set({ position: 0 });
      return;
    }

    if (shuffle) {
      // Rebuild if the order is stale — the queue may have changed underneath.
      const order =
        shuffleOrder.length === queue.length
          ? shuffleOrder
          : buildShuffleOrder(queue.length, index);

      const at = shufflePos + 1;
      if (at < order.length) {
        set({
          index: order[at],
          shuffleOrder: order,
          shufflePos: at,
          position: 0,
          playing: true,
        });
        return;
      }

      // The cycle is finished: every track has played exactly once.
      if (repeat === "all") {
        // No anchor, so the next pass may start anywhere — except on the track
        // that just finished, which would be a consecutive repeat.
        const next = buildShuffleOrder(queue.length, -1, index);
        set({
          index: next[0],
          shuffleOrder: next,
          shufflePos: 0,
          position: 0,
          playing: true,
        });
        return;
      }
      set({ playing: false, shuffleOrder: order });
      return;
    }

    const at = index + 1;
    if (at < queue.length) {
      set({ index: at, position: 0, playing: true });
    } else if (repeat === "all") {
      set({ index: 0, position: 0, playing: true });
    } else {
      set({ playing: false });
    }
  },

  previous: () => {
    const { index, position, shuffle, shuffleOrder, shufflePos } = get();
    // Matches every other player: "previous" restarts the current track unless
    // you press it in the first few seconds.
    if (position > 3) {
      set({ position: 0 });
      return;
    }

    // Shuffle steps back through the order actually played, rather than to
    // whatever happens to sit before this track in the queue.
    if (shuffle && shuffleOrder.length > 0) {
      const at = Math.max(0, shufflePos - 1);
      set({ index: shuffleOrder[at], shufflePos: at, position: 0, playing: true });
      return;
    }

    set({ index: Math.max(0, index - 1), position: 0, playing: true });
  },

  setRepeat: (repeat) => set({ repeat }),

  toggleShuffle: () =>
    set((s) => {
      const shuffle = !s.shuffle;
      if (!shuffle) {
        // Turning shuffle off keeps the current track and resumes in queue
        // order from wherever it sits.
        return { shuffle, shuffleOrder: [], shufflePos: 0 };
      }
      // Turning it on anchors the order to what is playing now, so enabling
      // shuffle never interrupts the current track.
      return {
        shuffle,
        shuffleOrder: buildShuffleOrder(s.queue.length, s.index),
        shufflePos: 0,
      };
    }),

  setVolume: (volume) => set({ volume }),
  _sync: (patch) => set(patch),
}));

export function streamURL(trackId: string, queueId = ""): string {
  // A YouTube item is served from the cache rather than a library root, and is
  // keyed by video id rather than track id.
  if (queueId.startsWith("youtube:")) {
    return `/api/v1/youtube/${trackId}/stream`;
  }
  return `/api/v1/tracks/${trackId}/stream`;
}

export function artURL(coverArtId: string): string {
  return `/api/v1/art/${coverArtId}`;
}

export function formatTime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "0:00";
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return `${m}:${s.toString().padStart(2, "0")}`;
}
