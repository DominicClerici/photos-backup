import { Gallery } from "@/components/Gallery";

// Rendered on the client: every byte comes from photod, the archive is private,
// and there is nothing to pre-render for a crawler. Server rendering here would
// only put Next between the browser and an API it can reach itself.
export default function Page() {
  return <Gallery />;
}
