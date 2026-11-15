import { GeistMono } from 'geist/font/mono';
import { GeistSans } from 'geist/font/sans';

import type { Metadata, Viewport } from 'next';
import type { ReactNode } from 'react';

import { site } from '@/lib/site';

import './globals.css';

export const metadata: Metadata = {
    metadataBase: site.url,
    title: {
        default: `${site.name} — ${site.tagline}`,
        template: `%s · ${site.name}`
    },
    description: site.description,
    applicationName: site.name,
    keywords: [
        'GitHub Actions',
        'self-hosted runners',
        'ephemeral runners',
        'Tart',
        'Podman',
        'Apple silicon',
        'CI/CD',
        'runner scale sets'
    ],
    alternates: { canonical: '/' },
    openGraph: {
        type: 'website',
        url: '/',
        siteName: site.name,
        title: `${site.name} — ${site.tagline}`,
        description: site.description,
        locale: 'en'
    },
    twitter: {
        card: 'summary_large_image',
        title: `${site.name} — ${site.tagline}`,
        description: site.description
    },
    formatDetection: { telephone: false, email: false, address: false }
};

export const viewport: Viewport = {
    colorScheme: 'dark light',
    themeColor: [
        { media: '(prefers-color-scheme: dark)', color: '#0a1515' },
        { media: '(prefers-color-scheme: light)', color: '#f3f8f5' }
    ]
};

const RootLayout = ({ children }: Readonly<{ children: ReactNode }>) => (
    <html lang="en" className={`${GeistSans.variable} ${GeistMono.variable}`}>
        <body className="min-h-dvh font-sans">
            <a
                href="#main"
                className="sr-only focus:not-sr-only focus:fixed focus:top-3 focus:left-3 focus:z-50 focus:rounded-lg focus:bg-accent-solid focus:px-4 focus:py-2 focus:text-accent-on"
            >
                Skip to content
            </a>
            {children}
        </body>
    </html>
);

export default RootLayout;
