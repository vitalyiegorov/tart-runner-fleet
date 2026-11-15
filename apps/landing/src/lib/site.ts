const repository = 'https://github.com/vitalyiegorov/tart-runner-fleet';

export const site = {
    name: 'Unyoke',
    tagline: 'CI/CD, on your own terms.',
    description:
        'Unyoke runs GitHub Actions on your own macOS and Linux machines, with ephemeral runners, LAN caches and a local artifact store, keeping CI/CD resources under your control with fail-closed scheduling.',
    url: new URL(process.env.NEXT_PUBLIC_SITE_URL ?? 'https://unyoke.vercel.app'),
    repository,
    docs: `${repository}#readme`,
    owner: 'https://github.com/vitalyiegorov'
} as const;

/** A file on the default branch of the repository, for evidence links. */
export const repoFile = (path: string): string => `${repository}/blob/main/${path}`;

export const adr = (file: string): string => repoFile(`docs/adr/${file}.md`);
