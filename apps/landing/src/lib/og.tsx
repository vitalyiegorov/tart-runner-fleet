import { readFile } from 'node:fs/promises';
import { join } from 'node:path';

import { ImageResponse } from 'next/og';

import { MARK_BAR, MARK_U, WORDMARK } from '@/components/brand';
import { site } from '@/lib/site';

export const ogSize = { width: 1200, height: 630 };
export const ogAlt = `${site.name}: ${site.tagline}`;

const font = (file: string) => readFile(join(process.cwd(), 'node_modules/geist/dist/fonts/geist-sans', file));

/** The social card: brand mark, name and tagline on the ink ground. */
export const renderOgImage = async () => {
    const [semibold, regular] = await Promise.all([font('Geist-SemiBold.ttf'), font('Geist-Regular.ttf')]);
    return new ImageResponse(
        <div
            style={{
                width: '100%',
                height: '100%',
                display: 'flex',
                flexDirection: 'column',
                justifyContent: 'space-between',
                padding: 72,
                background: 'radial-gradient(circle at 85% 10%, rgba(82,214,179,0.22), transparent 55%), #0a1515',
                color: '#f3f8f5',
                fontFamily: 'Geist'
            }}
        >
            <svg width="616" height="154" viewBox="0 0 352 88">
                <path transform="translate(4 12)" fill="#52d6b3" d={`${MARK_U} ${MARK_BAR}`} />
                <path
                    transform="translate(96 12)"
                    fill="none"
                    stroke="#f3f8f5"
                    strokeWidth="6"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    d={WORDMARK}
                />
            </svg>
            <div style={{ display: 'flex', flexDirection: 'column', gap: 20 }}>
                <div style={{ fontSize: 68, fontWeight: 600, letterSpacing: -2, color: '#52d6b3' }}>{site.tagline}</div>
                <div style={{ fontSize: 32, fontWeight: 400, color: '#a9c2bb', maxWidth: 980, lineHeight: 1.35 }}>
                    GitHub Actions on macOS and Linux machines you own: ephemeral runners, LAN caches and a local artifact store.
                </div>
            </div>
        </div>,
        {
            ...ogSize,
            fonts: [
                { name: 'Geist', data: semibold, weight: 600, style: 'normal' },
                { name: 'Geist', data: regular, weight: 400, style: 'normal' }
            ]
        }
    );
};
