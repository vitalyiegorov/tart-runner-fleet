import type { ReactNode } from 'react';

interface SectionHeadingProps {
    id: string;
    index: string;
    eyebrow: string;
    title: ReactNode;
    children?: ReactNode;
}

export const SectionHeading = ({ id, index, eyebrow, title, children }: SectionHeadingProps) => (
    <header className="reveal mx-auto max-w-3xl text-center">
        <p className="eyebrow">
            <span aria-hidden="true">{index} / </span>
            {eyebrow}
        </p>
        <h2 id={id} className="mt-4 text-3xl font-semibold tracking-tight text-balance sm:text-4xl lg:text-5xl">
            {title}
        </h2>
        {children === undefined ? null : <p className="mt-5 text-base leading-relaxed text-pretty text-fg-muted sm:text-lg">{children}</p>}
    </header>
);
