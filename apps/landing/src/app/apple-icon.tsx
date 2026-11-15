import { ImageResponse } from 'next/og';

import { MARK_BAR, MARK_U } from '@/components/brand';

export const size = { width: 180, height: 180 };
export const contentType = 'image/png';

/** iOS ignores SVG icons, so the mark is rendered on a chalk tile. */
const AppleIcon = () =>
    new ImageResponse(
        <div
            style={{
                width: '100%',
                height: '100%',
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'center',
                background: '#f3f8f5'
            }}
        >
            <svg width="132" height="132" viewBox="0 0 64 64">
                <path fill="#00856a" d={`${MARK_U} ${MARK_BAR}`} />
            </svg>
        </div>,
        size
    );

export default AppleIcon;
