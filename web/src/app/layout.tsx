import type { Metadata, Viewport } from "next";

import "./globals.css";
import { Geist } from "next/font/google";
import { SelectionProvider } from "@/hooks/useSelection";
import { TabBar } from "@/components/TabBar";
import { Toaster } from "@/components/ui/toast";
import { SignIn } from "@/components/SignIn";
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
      {/* Toasts are here for a related reason and a stronger one: a delete
          reloads the timeline it happened on, so the component that started it
          may well be unmounted by the time the toast is showing. The undo it
          offers has to outlive the grid it came from. */}
      {/* And the vault's password prompt is here because it is asked for from
          everywhere: a right-click in the library, a menu on an album tile, a
          page that has just found out it is locked. One dialog opened from a
          module-level subscription beats the same callback threaded through
          every component between here and those three. */}
      <body>
        <Toaster>
          <SelectionProvider>
            {children}
            <TabBar />
            <VaultGate />
            {/* browser gate: the gallery password, when photod is the one
                serving this app to a network rather than to this machine. It
                draws nothing at all otherwise. */}
            <SignIn />
          </SelectionProvider>
        </Toaster>
      </body>
    </html>
  );
}
