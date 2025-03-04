import "@/styles/tailwind/tailwind.css";
import { Inter } from "next/font/google";
import localFont from "next/font/local";

const inter = Inter({
  subsets: ["latin"],
  variable: "--font-inter",
});

const pangea = localFont({
  src: "../public/fonts/PangeaAfrikanTrial-Medium.woff2",
  variable: "--font-pangea",
});
export const metadata = {
  metadataBase: new URL("https://unkey.dev"),
  title: "Open Source API Key Management",
  description: "Accelerate your API development",
  openGraph: {
    title: "Open Source API Key Management",
    description: "Accelerate your API development",
    url: "https://unkey.dev",
    siteName: "unkey.dev",
    image: "https://unkey.dev/og.png",
  },
  twitter: {
    title: "Unkey",
    card: "summary_large_image",
    image: "https://unkey.dev/og.png",
  },
  robots: {
    index: true,
    follow: true,
    nocache: true,
    googleBot: {
      index: true,
      follow: false,
      noimageindex: true,
      "max-video-preview": -1,
      "max-image-preview": "large",
      "max-snippet": -1,
    },
  },
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className={[inter.variable, pangea.variable].join(" ")}>
      <head>
        <script
          defer
          data-domain={process.env.NEXT_PUBLIC_PLAUSIBLE_DOMAIN || ""}
          src="https://plausible.io/js/script.exclusions.js"
          data-exclude="/app/api*, /app/api*/*"
        />
      </head>
      <body>{children}</body>
    </html>
  );
}
