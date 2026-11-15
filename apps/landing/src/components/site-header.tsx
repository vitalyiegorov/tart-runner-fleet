import { Lockup } from '@/components/brand';
import { GitHubIcon } from '@/components/icons';
import { site } from '@/lib/site';

const links = [
    { href: '#how-it-works', label: 'How it works' },
    { href: '#features', label: 'Features' },
    { href: '#why', label: 'Why' },
    { href: '#quick-start', label: 'Quick start' }
];

export const SiteHeader = () => (
    <header className="sticky top-0 z-40 border-b border-line/60 bg-bg/75 backdrop-blur-xl supports-[backdrop-filter]:bg-bg/60">
        <div className="mx-auto flex h-16 max-w-6xl items-center justify-between gap-6 px-4 sm:px-6">
            <a href="#top" className="-m-1 rounded-md p-1" aria-label="Unyoke, back to top">
                <Lockup className="h-7 w-auto" />
            </a>
            <nav aria-label="Primary" className="hidden md:block">
                <ul className="flex items-center gap-1 text-sm text-fg-muted">
                    {links.map(link => (
                        <li key={link.href}>
                            <a href={link.href} className="rounded-md px-3 py-2 transition-colors hover:text-fg">
                                {link.label}
                            </a>
                        </li>
                    ))}
                </ul>
            </nav>
            <a
                href={site.repository}
                className="inline-flex items-center gap-2 rounded-lg border border-line-strong px-3 py-1.5 text-sm font-medium transition-colors hover:border-accent hover:text-accent-text"
            >
                <GitHubIcon className="size-4" />
                <span>GitHub</span>
            </a>
        </div>
    </header>
);
