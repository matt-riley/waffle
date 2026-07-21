import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const websiteRoot = new URL('../', import.meta.url);

async function read(relativePath) {
	try {
		return await readFile(new URL(relativePath, websiteRoot), 'utf8');
	} catch (error) {
		if (error?.code === 'ENOENT') return '';
		throw error;
	}
}

function navDestinations(html, label) {
	const nav = html.match(new RegExp(`<nav aria-label="${label}"[\\s\\S]*?<\\/nav>`));
	assert.ok(nav, `${label} navigation is present`);

	return [...nav[0].matchAll(/href="([^"]+)"/g)].map((match) => match[1]).sort();
}

test('motion enhancement fails open when the GSAP module does not become ready', async () => {
	const [layout, motion] = await Promise.all([
		read('src/layouts/Layout.astro'),
		read('src/scripts/motion.ts'),
	]);

	assert.match(layout, /setTimeout/);
	assert.match(layout, /js-motion-ready/);
	assert.match(layout, /classList\.remove\('js-motion'\)/);
	assert.match(layout, /removeProperty\('opacity'\)/);
	assert.match(motion, /classList\.add\('js-motion-ready'\)/);
});

test('motion enhancement stays disabled after the fail-open guard runs', async () => {
	const motion = await read('src/scripts/motion.ts');

	assert.match(
		motion,
		/if \(prefersReducedMotion\(\)\) \{[\s\S]*?return;[\s\S]*?\}\s*if \(!root\.classList\.contains\('js-motion'\)\) return;\s*gsap\.registerPlugin/,
	);
	assert.doesNotMatch(motion, /root\.classList\.add\('js-motion'\)/);
});

test('navigation labels and section ids describe their destinations', async () => {
	const [header, footer, activities, namesake] = await Promise.all([
		read('src/components/SiteHeader.astro'),
		read('src/components/SiteFooter.astro'),
		read('src/components/WhatSheGetsInto.astro'),
		read('src/components/WhyWaffle.astro'),
	]);

	assert.doesNotMatch(header, /label:\s*'Notes'/);
	assert.match(header, /href:\s*'#why-waffle'.*label:\s*'Why Waffle'/);
	assert.match(footer, /href="#why-waffle"[^>]*>Why Waffle</);
	assert.match(activities, /id="what-she-does"/);
	assert.match(namesake, /id="why-waffle"/);
});

test('header and footer navigation expose the same destinations', async () => {
	const builtHome = await read('dist/index.html');

	assert.deepEqual(navDestinations(builtHome, 'Footer'), navDestinations(builtHome, 'Primary'));
});

test('approved personal copy appears on the page and in the copy document', async () => {
	const [builtHome, copy] = await Promise.all([read('dist/index.html'), read('COPY.md')]);

	for (const content of [builtHome, copy]) {
		assert.match(content, /One binary\. One very orange accomplice\./);
		assert.match(content, /I wrote the code\. She supplied the personality\./);
		assert.doesNotMatch(content, /A project that lives here\. A name that meows back\./);
	}
});

test('shared layout exposes a skip link and every page exposes the main target', async () => {
	const [layout, home, notFound] = await Promise.all([
		read('src/layouts/Layout.astro'),
		read('src/pages/index.astro'),
		read('src/pages/404.astro'),
	]);

	assert.match(layout, /href="#main-content"[^>]*>\s*Skip to content/);
	assert.match(home, /<main id="main-content" tabindex="-1"/);
	assert.match(notFound, /<main id="main-content" tabindex="-1"/);
});

test('layout emits canonical and social sharing metadata from a configurable site origin', async () => {
	const [layout, config, builtHome] = await Promise.all([
		read('src/layouts/Layout.astro'),
		read('astro.config.mjs'),
		read('dist/index.html'),
	]);

	assert.match(config, /PUBLIC_SITE_URL/);
	assert.match(layout, /rel="canonical"/);
	assert.match(layout, /property="og:title"/);
	assert.match(layout, /property="og:description"/);
	assert.match(layout, /property="og:image"/);
	assert.match(layout, /property="og:url"/);
	assert.match(layout, /name="twitter:card"/);
	assert.match(builtHome, /<meta property="og:title"/);
});

test('brand and navigation links provide 44px hit areas', async () => {
	const [header, styles] = await Promise.all([
		read('src/components/SiteHeader.astro'),
		read('src/styles/global.css'),
	]);

	assert.match(header, /class="brand-link/);
	assert.match(styles, /\.brand-link\s*\{[^}]*min-block-size:\s*2\.75rem;[^}]*min-inline-size:\s*2\.75rem;/s);
	assert.match(styles, /\.nav-link\s*\{[^}]*min-height:\s*2\.75rem;[^}]*min-width:\s*2\.75rem;/s);
});

test('the static build contains a branded 404 route with a path home', async () => {
	const [notFoundSource, notFoundBuild] = await Promise.all([
		read('src/pages/404.astro'),
		read('dist/404.html'),
	]);

	assert.match(notFoundSource, /Nothing to see here/);
	assert.match(notFoundSource, /href="\/"/);
	assert.match(notFoundBuild, /Nothing to see here/);
	assert.match(notFoundBuild, /Back home/);
});

test('new-tab links declare both opener and referrer protections', async () => {
	const files = await Promise.all([
		read('src/components/SiteHeader.astro'),
		read('src/components/SiteFooter.astro'),
		read('src/components/SoftClose.astro'),
	]);

	for (const source of files) {
		assert.match(source, /target="_blank"[^>]*rel="noopener noreferrer"/s);
	}
});

test('the website package cannot be published accidentally', async () => {
	const packageJSON = JSON.parse(await read('package.json'));

	assert.equal(packageJSON.private, true);
});

test('a blank configured site origin is treated as unset', () => {
	const configURL = new URL('astro.config.mjs', websiteRoot).href;
	const output = execFileSync(
		process.execPath,
		[
			'--input-type=module',
			'--eval',
			`const { default: config } = await import(${JSON.stringify(configURL)}); process.stdout.write(JSON.stringify(config.site ?? null));`,
		],
		{
			encoding: 'utf8',
			env: { ...process.env, PUBLIC_SITE_URL: '   ' },
		},
	);

	assert.equal(JSON.parse(output), null);
});
