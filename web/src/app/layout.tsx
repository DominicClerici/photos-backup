import type { Metadata, Viewport } from "next";

import "./globals.css";
import { Geist } from "next/font/google";
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
      <body>{children}</body>
    </html>
  );
}
