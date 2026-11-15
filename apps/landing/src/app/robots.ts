import type { MetadataRoute } from 'next';

import { site } from '@/lib/site';

const robots = (): MetadataRoute.Robots => ({
    rules: [{ userAgent: '*', allow: '/' }],
    sitemap: new URL('/sitemap.xml', site.url).href,
    host: site.url.origin
});

export default robots;
