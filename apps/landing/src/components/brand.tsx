/*
 * Path data is copied verbatim from assets/brand/*.svg; the e2e suite asserts
 * that it still matches. Colour comes from the theme tokens instead of being
 * baked in, so one drawing serves the light and dark lockups.
 */
export const MARK_U = 'M8 8h12v24a12 12 0 0 0 24 0V24h12v8a24 24 0 0 1-48 0Z';
export const MARK_BAR = 'M44 8h12v8H44Z';
export const WORDMARK =
    'M0 4v29c0 18 28 18 28 0V4 M42 46V18m0 12c0-17 26-17 26 0v16 M82 18l13 28m13-28L90 62 M123 32c0-19 28-19 28 0s-28 19-28 0Z M166 4v42m24-28-24 19m10-8 16 17 M207 32h28c0-19-28-19-28 0 0 17 19 19 28 9';

interface MarkProps {
    className?: string;
    animated?: boolean;
}

export const Mark = ({ className, animated = false }: MarkProps) => (
    <svg viewBox="0 0 64 64" className={className} aria-hidden="true" focusable="false" data-brand="mark">
        <path fill="var(--accent)" d={MARK_U} />
        <path fill="var(--accent)" d={MARK_BAR} className={animated ? 'motion-release' : undefined} />
    </svg>
);

export const Lockup = ({ className }: { className?: string }) => (
    <svg viewBox="0 0 352 88" className={className} role="img" aria-label="Unyoke" data-brand="lockup">
        <path transform="translate(4 12)" fill="var(--accent)" d={`${MARK_U} ${MARK_BAR}`} />
        <path
            transform="translate(96 12)"
            fill="none"
            stroke="var(--fg)"
            strokeWidth="6"
            strokeLinecap="round"
            strokeLinejoin="round"
            d={WORDMARK}
        />
    </svg>
);
