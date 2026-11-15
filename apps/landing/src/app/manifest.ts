import type { MetadataRoute } from 'next';

import { site } from '@/lib/site';

const manifest = (): MetadataRoute.Manifest => ({
    name: site.name,
    short_name: site.name,
    description: site.description,
    start_url: '/',
    display: 'browser',
    background_color: '#0a1515',
    theme_color: '#0a1515',
    icons: [
        { src: '/icon.svg', sizes: 'any', type: 'image/svg+xml' },
        { src: '/apple-icon', sizes: '180x180', type: 'image/png' }
    ]
});

export default manifest;
