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

  playQueue: (tracks, startAt = 0, queueId = "") =>
    set({ queue: tracks, queueId, index: startAt, playing: true, position: 0, duration: 0 }),

  toggle: () => set((s) => ({ playing: !s.playing })),

  next: () => {
    const { queue, index, repeat, shuffle } = get();
    if (queue.length === 0) return;

    // Repeat-one restarts the same track rather than advancing; the audio
    // element handles the actual rewind when the src does not change.
    if (repeat === "one") {
      set({ position: 0 });
      return;
    }
    if (shuffle) {
      set({ index: Math.floor(Math.random() * queue.length), position: 0, playing: true });
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
    const { index, position } = get();
    // Matches every other player: "previous" restarts the current track unless
    // you press it in the first few seconds.
    if (position > 3) {
      set({ position: 0 });
      return;
    }
    set({ index: Math.max(0, index - 1), position: 0, playing: true });
  },

  setRepeat: (repeat) => set({ repeat }),
  toggleShuffle: () => set((s) => ({ shuffle: !s.shuffle })),
  setVolume: (volume) => set({ volume }),
  _sync: (patch) => set(patch),
}));

export function streamURL(trackId: string): string {
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
