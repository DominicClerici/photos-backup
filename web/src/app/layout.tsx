import type { Metadata, Viewport } from "next";

import "./globals.css";
import { Geist } from "next/font/google";
import { SelectionProvider } from "@/hooks/useSelection";
import { ViewProvider } from "@/hooks/useView";
import { CommandPalette } from "@/components/CommandPalette";
import { TabBar } from "@/components/TabBar";
import { Toaster } from "@/components/ui/toast";
import { VaultGate } from "@/components/VaultGate";
import { cn } from "@/lib/utils";

const geist = Geist({subsets:['latin'],variable:'--font-sans'});

export const metadata: Metadata = {
  title: "Photos",
  description: "Self-hosted photo and video archive",
};

export const viewport: Viewport = {
  themeColor: "#0b0b0d",
  // The viewer is a full-screen surface; letting it zoom on a phone fights the
  // pinch gestures the browser already gives the image.
  width: "device-width",
  initialScale: 1,
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    // `dark` is unconditional: the app has no light theme, but the shadcn
    // components' own dark: variants only match inside it.
    <html lang="en" className={cn("dark font-sans", geist.variable)}>
      {/* The bar lives here rather than in each page so that switching sections
          leaves it mounted: it keeps its highlight and never blinks out mid-
          navigation. `children` stays a server component either way. */}
      {/* The selection lives above both of them: the grid that fills it is
          mounted by a page and the control that reports it by the bar, so this
          is the only place the two can meet. `children` stays a server
          component — it is passed through, not rendered by, the provider. */}
      {/* The sort and the filters are the same arrangement for the same reason,
          and one layer further in: a view is of a selection's timeline, and
          changing it drops the selection made under the old one. */}
      {/* Toasts are here for a related reason and a stronger one: a delete
          reloads the timeline it happened on, so the component that started it
          may well be unmounted by the time the toast is showing. The undo it
          offers has to outlive the grid it came from. */}
      {/* The command palette is here for the same reason as the prompt below it,
          and one more: ⌘K has to work on a page that is still loading and on one
          that has nothing to do with searching, so the listener has to outlive
          every page. It is also where the Search tab goes — see TabBar. */}
      {/* And the vault's password prompt is here because it is asked for from
          everywhere: a right-click in the library, a menu on an album tile, a
          page that has just found out it is locked. One dialog opened from a
          module-level subscription beats the same callback threaded through
          every component between here and those three. */}
      <body>
        <Toaster>
          <SelectionProvider>
            <ViewProvider>
              {children}
              <TabBar />
              <CommandPalette />
              <VaultGate />
            </ViewProvider>
          </SelectionProvider>
        </Toaster>
      </body>
    </html>
  );
}
