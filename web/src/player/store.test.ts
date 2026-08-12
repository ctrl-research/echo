import { beforeEach, describe, expect, it } from "vitest";
import { buildShuffleOrder, usePlayer, type Track } from "./store";

function tracks(n: number): Track[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `t${i}`,
    title: `Track ${i}`,
    artistName: "Artist",
    albumName: "Album",
    genres: [],
    suffix: "mp3",
    overridden: false,
    favorite: false,
  })) as Track[];
}

/** Plays the queue forward `steps` times, recording the index each time. */
function walk(steps: number): number[] {
  const seen = [usePlayer.getState().index];
  for (let i = 0; i < steps; i++) {
    usePlayer.getState().next();
    seen.push(usePlayer.getState().index);
  }
  return seen;
}

beforeEach(() => {
  usePlayer.setState({
    queue: [],
    queueId: "",
    index: 0,
    playing: false,
    position: 0,
    duration: 0,
    repeat: "off",
    shuffle: false,
    shuffleOrder: [],
    shufflePos: 0,
  });
});

describe("buildShuffleOrder", () => {
  it("is a permutation: every index exactly once", () => {
    for (const n of [2, 3, 5, 20]) {
      const order = buildShuffleOrder(n, 0);
      expect(order).toHaveLength(n);
      expect([...order].sort((a, b) => a - b)).toEqual(
        Array.from({ length: n }, (_, i) => i),
      );
    }
  });

  it("starts on the anchor, so enabling shuffle does not interrupt playback", () => {
    for (let run = 0; run < 50; run++) {
      expect(buildShuffleOrder(10, 4)[0]).toBe(4);
    }
  });

  it("keeps the avoided index out of first place", () => {
    // Repeated because the failure is probabilistic: a naive implementation
    // passes roughly nine times in ten.
    for (let run = 0; run < 200; run++) {
      expect(buildShuffleOrder(10, -1, 7)[0]).not.toBe(7);
    }
  });

  it("handles degenerate lengths", () => {
    expect(buildShuffleOrder(0, 0)).toEqual([]);
    expect(buildShuffleOrder(1, 0)).toEqual([0]);
  });
});

describe("shuffle playback", () => {
  it("never plays the same track twice in a row", () => {
    // The reported bug: picking at random each time could land on the track
    // already playing.
    for (let run = 0; run < 100; run++) {
      usePlayer.setState({ repeat: "all", shuffle: false });
      usePlayer.getState().playQueue(tracks(5), 0, "list");
      usePlayer.getState().toggleShuffle();

      const seen = walk(40);
      for (let i = 1; i < seen.length; i++) {
        expect(seen[i], `repeat at step ${i} in run ${run}: ${seen.join(",")}`)
          .not.toBe(seen[i - 1]);
      }
    }
  });

  it("plays every track once before repeating any", () => {
    usePlayer.setState({ shuffle: false });
    usePlayer.getState().playQueue(tracks(8), 0, "list");
    usePlayer.getState().toggleShuffle();

    const cycle = walk(7);
    expect(new Set(cycle).size).toBe(8);
  });

  it("does not repeat across a cycle boundary", () => {
    // The subtle case: the last track of one pass must not open the next.
    for (let run = 0; run < 100; run++) {
      usePlayer.setState({ repeat: "all", shuffle: false });
      usePlayer.getState().playQueue(tracks(4), 0, "list");
      usePlayer.getState().toggleShuffle();

      const seen = walk(4); // one full cycle plus the wrap
      expect(seen[4]).not.toBe(seen[3]);
    }
  });

  it("stops at the end of a cycle when repeat is off", () => {
    usePlayer.setState({ repeat: "off", shuffle: false });
    usePlayer.getState().playQueue(tracks(3), 0, "list");
    usePlayer.getState().toggleShuffle();

    walk(3);
    expect(usePlayer.getState().playing).toBe(false);
  });

  it("keeps playing the current track when shuffle is switched on", () => {
    usePlayer.getState().playQueue(tracks(10), 6, "list");
    usePlayer.getState().toggleShuffle();
    expect(usePlayer.getState().index).toBe(6);
  });

  it("steps back through the order actually played", () => {
    usePlayer.setState({ shuffle: false });
    usePlayer.getState().playQueue(tracks(6), 0, "list");
    usePlayer.getState().toggleShuffle();

    const forward = walk(3);
    usePlayer.getState().previous();
    expect(usePlayer.getState().index).toBe(forward[2]);
    usePlayer.getState().previous();
    expect(usePlayer.getState().index).toBe(forward[1]);
  });

  it("resumes queue order when shuffle is switched off", () => {
    usePlayer.getState().playQueue(tracks(5), 0, "list");
    usePlayer.getState().toggleShuffle();
    usePlayer.getState().toggleShuffle();

    const at = usePlayer.getState().index;
    usePlayer.getState().next();
    expect(usePlayer.getState().index).toBe(at + 1);
  });

  it("survives a single-track queue", () => {
    usePlayer.setState({ repeat: "all" });
    usePlayer.getState().playQueue(tracks(1), 0, "list");
    usePlayer.getState().toggleShuffle();

    // Nothing else to play, so the same track is the only possible answer.
    usePlayer.getState().next();
    expect(usePlayer.getState().index).toBe(0);
  });
});

describe("repeat", () => {
  it("repeat-one replays the same track, which is the point", () => {
    // The listener asked for a consecutive repeat here, so shuffle's rule
    // must not override it.
    usePlayer.setState({ repeat: "one", shuffle: true });
    usePlayer.getState().playQueue(tracks(5), 2, "list");
    const before = usePlayer.getState().index;

    usePlayer.getState().next();
    expect(usePlayer.getState().index).toBe(before);
    expect(usePlayer.getState().position).toBe(0);
  });

  it("advances in queue order without shuffle", () => {
    usePlayer.getState().playQueue(tracks(4), 0, "list");
    expect(walk(3)).toEqual([0, 1, 2, 3]);
  });

  it("wraps with repeat-all without shuffle", () => {
    usePlayer.setState({ repeat: "all" });
    usePlayer.getState().playQueue(tracks(3), 0, "list");
    expect(walk(3)).toEqual([0, 1, 2, 0]);
  });
});

describe("previous", () => {
  it("restarts the track when pressed after a few seconds", () => {
    usePlayer.getState().playQueue(tracks(4), 2, "list");
    usePlayer.getState()._sync({ position: 10 });

    usePlayer.getState().previous();
    expect(usePlayer.getState().index).toBe(2);
    expect(usePlayer.getState().position).toBe(0);
  });

  it("goes back a track when pressed early", () => {
    usePlayer.getState().playQueue(tracks(4), 2, "list");
    usePlayer.getState()._sync({ position: 1 });

    usePlayer.getState().previous();
    expect(usePlayer.getState().index).toBe(1);
  });
});
