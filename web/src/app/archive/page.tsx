import { VaultView } from "@/components/VaultView";

// Its own route rather than /collections/archive, for the reason /trash has its
// own: a collection is a slice of the library, and this is what has left it —
// encrypted, out of every album, and unreadable without a password.
//
// The two buckets get two routes rather than one parameterised one because they
// are two destinations somebody types, links to and bookmarks. /vault/archive
// would be an implementation detail wearing a URL.
export default function Page() {
  return <VaultView bucket="archive" />;
}
