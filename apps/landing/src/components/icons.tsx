import type { ReactNode } from 'react';

const Icon = ({ children, className = 'size-5' }: { children: ReactNode; className?: string | undefined }) => (
    <svg
        viewBox="0 0 24 24"
        className={className}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        focusable="false"
    >
        {children}
    </svg>
);

export const GitHubIcon = ({ className = 'size-5' }: { className?: string }) => (
    <svg viewBox="0 0 24 24" className={className} fill="currentColor" aria-hidden="true" focusable="false">
        <path d="M12 .5a11.5 11.5 0 0 0-3.64 22.41c.58.1.79-.25.79-.56v-2c-3.2.7-3.88-1.37-3.88-1.37-.52-1.33-1.28-1.69-1.28-1.69-1.05-.71.08-.7.08-.7 1.16.08 1.77 1.19 1.77 1.19 1.03 1.77 2.7 1.26 3.36.96.1-.75.4-1.26.73-1.55-2.55-.29-5.24-1.28-5.24-5.69 0-1.26.45-2.29 1.19-3.09-.12-.29-.52-1.46.11-3.05 0 0 .97-.31 3.17 1.18a11 11 0 0 1 5.77 0c2.2-1.49 3.17-1.18 3.17-1.18.63 1.59.23 2.76.11 3.05.74.8 1.19 1.83 1.19 3.09 0 4.42-2.7 5.39-5.26 5.68.41.36.78 1.06.78 2.14v3.17c0 .31.21.67.8.56A11.5 11.5 0 0 0 12 .5Z" />
    </svg>
);

export const ArrowIcon = ({ className }: { className?: string }) => (
    <Icon className={className}>
        <path d="M5 12h14M13 6l6 6-6 6" />
    </Icon>
);

export const BookIcon = ({ className }: { className?: string }) => (
    <Icon className={className}>
        <path d="M4 5.5A2.5 2.5 0 0 1 6.5 3H20v15H6.5A2.5 2.5 0 0 0 4 20.5zM4 20.5A2.5 2.5 0 0 0 6.5 23H20v-5" />
    </Icon>
);

export const featureIcons = {
    ephemeral: (
        <Icon>
            <rect x="4" y="5" width="16" height="11" rx="2" />
            <path d="M8 20h8M12 16v4" />
            <path d="m9.5 9 5 3M14.5 9l-5 3" />
        </Icon>
    ),
    fleet: (
        <Icon>
            <rect x="3" y="4" width="7" height="6" rx="1.5" />
            <rect x="14" y="4" width="7" height="6" rx="1.5" />
            <rect x="8.5" y="14" width="7" height="6" rx="1.5" />
            <path d="M6.5 10v2h11v-2M12 12v2" />
        </Icon>
    ),
    failClosed: (
        <Icon>
            <path d="M12 3 4.5 6v5.5c0 4.5 3.2 8.2 7.5 9.5 4.3-1.3 7.5-5 7.5-9.5V6z" />
            <path d="M9.5 12.5 11.3 14 15 10" />
        </Icon>
    ),
    cache: (
        <Icon>
            <rect x="3" y="4" width="18" height="7" rx="2" />
            <rect x="3" y="13" width="18" height="7" rx="2" />
            <path d="M7 7.5h.01M7 16.5h.01M11 7.5h6M11 16.5h6" />
        </Icon>
    ),
    artifact: (
        <Icon>
            <path d="M12 3 4 7v10l8 4 8-4V7z" />
            <path d="m4 7 8 4 8-4M12 11v10" />
        </Icon>
    ),
    cli: (
        <Icon>
            <rect x="3" y="4" width="18" height="16" rx="2.5" />
            <path d="m7 9 3 3-3 3M12.5 15H17" />
        </Icon>
    ),
    database: (
        <Icon>
            <path d="M4 6.5C4 5.1 7.6 4 12 4s8 1.1 8 2.5S16.4 9 12 9 4 7.9 4 6.5z" />
            <path d="M4 6.5v11C4 18.9 7.6 20 12 20s8-1.1 8-2.5v-11" />
            <path d="M4 12c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5" />
        </Icon>
    ),
    update: (
        <Icon>
            <path d="M20 11a8 8 0 0 0-14.6-4.5M4 4v3h3M4 13a8 8 0 0 0 14.6 4.5M20 20v-3h-3" />
        </Icon>
    ),
    licence: (
        <Icon>
            <path d="M7 3h7l5 5v13H7z" />
            <path d="M14 3v5h5M10 13h6M10 17h4" />
        </Icon>
    )
};
