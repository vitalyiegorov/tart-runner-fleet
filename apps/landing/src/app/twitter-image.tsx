import { ogAlt, ogSize, renderOgImage } from '@/lib/og';

export const alt = ogAlt;
export const size = ogSize;
export const contentType = 'image/png';

const TwitterImage = () => renderOgImage();

export default TwitterImage;
