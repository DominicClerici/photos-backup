import type { Metadata, Viewport } from "next";

import "./globals.css";

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
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
