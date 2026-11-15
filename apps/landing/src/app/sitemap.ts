import type { MetadataRoute } from 'next';

import { site } from '@/lib/site';

const sitemap = (): MetadataRoute.Sitemap => [{ url: new URL('/', site.url).href, changeFrequency: 'monthly', priority: 1 }];

export default sitemap;
