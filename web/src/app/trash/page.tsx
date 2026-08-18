import { TrashView } from "@/components/TrashView";

// Its own route rather than /collections/trash, because it is not one: a
// collection is a slice of the library, and this is what has left it. The row
// that leads here has always been under Other for the same reason.
export default function Page() {
  return <TrashView />;
}
