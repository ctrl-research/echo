import { useQuery } from "@tanstack/react-query";
import { clientFor } from "./client";
import type { components } from "./schema";
import { useSession } from "./SessionProvider";

export type Item = components["schemas"]["BaseItemDto"];

/**
 * Queries are keyed by server and user as well as by what they ask for.
 * Signing into a different account, or a different server, must not serve the
 * previous one's library out of cache — the ids would even collide, since they
 * are only unique within a server.
 */
function keyFor(server: string, userId: string, ...rest: unknown[]) {
  return ["jellyfin", server, userId, ...rest];
}

/** Unwraps a response, turning a transport or HTTP error into a thrown one. */
function unwrap<T>(res: { data?: T; error?: unknown }): T {
  if (res.error || res.data === undefined) {
    throw new Error("Request to Jellyfin failed");
  }
  return res.data;
}

/** Albums, alphabetical. */
export function useAlbums() {
  const { session } = useSession();
  return useQuery({
    enabled: !!session,
    queryKey: keyFor(session?.server ?? "", session?.userId ?? "", "albums"),
    queryFn: async () => {
      const s = session!;
      const res = await clientFor(s).GET("/Items", {
        params: {
          query: {
            userId: s.userId,
            includeItemTypes: ["MusicAlbum"],
            // Without recursive the query only returns children of the root,
            // which for music is the library folder rather than the albums.
            recursive: true,
            sortBy: ["SortName"],
            sortOrder: ["Ascending"],
          },
        },
      });
      return unwrap(res).Items ?? [];
    },
  });
}

/**
 * The tracks of one album, in playing order.
 *
 * Sorted by disc then track rather than by name: an album is an ordered thing,
 * and alphabetical order would shuffle it.
 */
export function useAlbumTracks(albumId: string | null) {
  const { session } = useSession();
  return useQuery({
    enabled: !!session && !!albumId,
    queryKey: keyFor(
      session?.server ?? "",
      session?.userId ?? "",
      "album",
      albumId,
    ),
    queryFn: async () => {
      const s = session!;
      const res = await clientFor(s).GET("/Items", {
        params: {
          query: {
            userId: s.userId,
            parentId: albumId!,
            includeItemTypes: ["Audio"],
            sortBy: ["ParentIndexNumber", "IndexNumber", "SortName"],
            sortOrder: ["Ascending"],
          },
        },
      });
      return unwrap(res).Items ?? [];
    },
  });
}
