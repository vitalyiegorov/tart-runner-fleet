import { Lockup } from '@/components/brand';
import { repoFile, site } from '@/lib/site';

const links = [
    { label: 'GitHub', href: site.repository },
    { label: 'AGPL-3.0 licence', href: repoFile('LICENSE') },
    { label: 'Contributor licence agreement', href: repoFile('CLA.md') },
    { label: 'Contributing', href: repoFile('CONTRIBUTING.md') },
    { label: 'Commercial licences', href: site.owner }
];

export const SiteFooter = () => (
    <footer className="border-t border-line/60">
        <div className="mx-auto flex max-w-6xl flex-col gap-10 px-4 py-14 sm:px-6 md:flex-row md:items-start md:justify-between">
            <div className="max-w-sm">
                <Lockup className="h-8 w-auto" />
                <p className="mt-4 text-sm text-fg-muted">{site.tagline}</p>
                <p className="mt-2 text-sm text-fg-subtle">Formerly Tart Runner Fleet.</p>
            </div>
            <nav aria-label="Footer">
                <ul className="grid gap-x-10 gap-y-3 text-sm sm:grid-cols-2">
                    {links.map(link => (
                        <li key={link.href}>
                            <a href={link.href} className="text-fg-muted transition-colors hover:text-fg">
                                {link.label}
                            </a>
                        </li>
                    ))}
                </ul>
            </nav>
        </div>
        <div className="border-t border-line/60">
            <p className="mx-auto max-w-6xl px-4 py-6 text-xs leading-relaxed text-fg-subtle sm:px-6">
                Unyoke is licensed under AGPL-3.0; commercial licences are available from the owner. Contributions require signing the CLA.
                GitHub and GitHub Actions are trademarks of GitHub, Inc.; Unyoke is not affiliated with or endorsed by GitHub.
            </p>
        </div>
    </footer>
);
