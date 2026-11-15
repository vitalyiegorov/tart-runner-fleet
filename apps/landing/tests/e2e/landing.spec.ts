import { readFile } from 'node:fs/promises';
import { join } from 'node:path';

import { AxeBuilder } from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

const brandDir = join(import.meta.dirname, '../../../../assets/brand');
const repository = 'https://github.com/vitalyiegorov/tart-runner-fleet';

for (const colorScheme of ['dark', 'light'] as const) {
    test.describe(`${colorScheme} scheme`, () => {
        test.use({ colorScheme, reducedMotion: 'reduce' });

        test('has no serious or critical accessibility violations', async ({ page }) => {
            await page.goto('/');
            const { violations } = await new AxeBuilder({ page })
                .withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'])
                .analyze();
            const blocking = violations.filter(violation => violation.impact === 'serious' || violation.impact === 'critical');
            expect(blocking.map(({ id, nodes }) => `${id}: ${nodes.map(node => node.target.join(' ')).join(', ')}`)).toEqual([]);
        });
    });
}

test('renders the page structure without errors or horizontal overflow', async ({ page }) => {
    const errors: string[] = [];
    page.on('pageerror', error => errors.push(error.message));
    page.on('console', message => {
        if (message.type() === 'error') {
            errors.push(message.text());
        }
    });

    const response = await page.goto('/');
    expect(response?.status()).toBe(200);
    await expect(page).toHaveTitle(/Unyoke/u);
    await expect(page.getByRole('heading', { level: 1 })).toHaveText('CI/CD, on your own terms.');
    await expect(page.getByRole('banner')).toBeVisible();
    await expect(page.getByRole('main')).toBeVisible();
    await expect(page.getByRole('contentinfo')).toBeVisible();
    const sections = ['how-it-works', 'features', 'why', 'quick-start'];
    await Promise.all(sections.map(id => expect(page.locator(`section[aria-labelledby="${id}"] h2#${id}`)).toHaveCount(1)));
    await expect(page.getByRole('link', { name: 'View on GitHub' })).toHaveAttribute('href', repository);
    await expect(page.getByRole('link', { name: 'Read the docs' })).toHaveAttribute('href', `${repository}#readme`);

    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);
    expect(errors).toEqual([]);
});

test('keeps the inline brand drawing identical to the shipped SVG', async ({ page, request }) => {
    const mark = await readFile(join(brandDir, 'logo-mark.svg'), 'utf8');
    const markPath = /\sd="([^"]+)"/u.exec(mark)?.[1];
    expect(markPath).toBeTruthy();

    await page.goto('/');
    const rendered = await page
        .locator('[data-brand="mark"] path')
        .evaluateAll(paths => paths.map(path => path.getAttribute('d')).join(' '));
    expect(rendered).toBe(markPath);

    const icon = await request.get('/icon.svg');
    expect(icon.ok()).toBe(true);
    expect(await icon.text()).toBe(mark);
});

test('serves metadata routes', async ({ page, request }) => {
    await page.goto('/');
    await expect(page.locator('link[rel="canonical"]')).toHaveAttribute('href', 'https://unyoke.dev');
    await expect(page.locator('meta[property="og:image"]')).toHaveAttribute('content', /\/opengraph-image/u);

    const routes = [
        ['/opengraph-image', 'image/png'],
        ['/twitter-image', 'image/png'],
        ['/apple-icon', 'image/png'],
        ['/robots.txt', 'text/plain'],
        ['/sitemap.xml', 'application/xml'],
        ['/manifest.webmanifest', 'application/manifest+json']
    ] as const;
    const responses = await Promise.all(routes.map(([path]) => request.get(path)));
    responses.forEach((response, index) => {
        const [path, type] = routes[index] ?? ['', ''];
        expect(response.status(), path).toBe(200);
        expect(response.headers()['content-type'], path).toContain(type);
    });
    expect(await (await request.get('/robots.txt')).text()).toContain('Sitemap: https://unyoke.dev/sitemap.xml');
});

test('is keyboard navigable from the skip link', async ({ page }) => {
    await page.goto('/');
    await page.keyboard.press('Tab');
    const skip = page.getByRole('link', { name: 'Skip to content' });
    await expect(skip).toBeFocused();
    await expect(skip).toBeVisible();
    await page.keyboard.press('Enter');
    await expect(page).toHaveURL(/#main$/u);
});
