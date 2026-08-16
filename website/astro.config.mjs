// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

import tailwindcss from '@tailwindcss/vite';

import { resolveSiteURL } from './src/site-url.mjs';

const site = resolveSiteURL(process.env.PUBLIC_SITE_URL);

// https://astro.build/config
export default defineConfig({
	site,
	integrations: [
		starlight({
			title: 'Waffle',
			description:
				'How to live with Waffle: a personal AI agent named after a small orange menace.',
			// The hand-built 404 in src/pages stays; Starlight must not inject its own.
			disable404Route: true,
			customCss: ['./src/styles/docs.css'],
			components: {
				SiteTitle: './src/components/docs/SiteTitle.astro',
			},
			head: [
				{ tag: 'link', attrs: { rel: 'preconnect', href: 'https://fonts.googleapis.com' } },
				{
					tag: 'link',
					attrs: { rel: 'preconnect', href: 'https://fonts.gstatic.com', crossorigin: true },
				},
				{
					tag: 'link',
					attrs: {
						rel: 'stylesheet',
						href: 'https://fonts.googleapis.com/css2?family=Nunito:ital,wght@0,400;0,600;0,700;0,800;1,400&family=Source+Serif+4:ital,opsz,wght@0,8..60,400;0,8..60,600;1,8..60,400&display=swap',
					},
				},
			],
			expressiveCode: {
				// Warmth comes from the card (see docs.css), not from a tinted syntax
				// theme: both of these ship contrast-checked token colours, and a
				// single theme recoloured twice is the usual way dark-mode code fails.
				themes: ['github-light', 'github-dark'],
				styleOverrides: {
					borderRadius: '0.5rem',
					borderColor: 'var(--waffle-code-border)',
					codeBackground: 'var(--waffle-code-bg)',
					frames: {
						editorTabBarBackground: 'var(--waffle-code-bg)',
						terminalTitlebarBackground: 'var(--waffle-code-bg)',
						terminalBackground: 'var(--waffle-code-bg)',
					},
				},
			},
			social: [
				{ icon: 'github', label: 'Source', href: 'https://github.com/matt-riley/waffle' },
			],
			sidebar: [
				{
					label: 'Meet Waffle',
					items: [
						{ label: 'What Waffle is', slug: 'docs/meet/what-waffle-is' },
						{ label: 'What she can do', slug: 'docs/meet/what-she-can-do' },
						{ label: 'Bringing her home', slug: 'docs/meet/bringing-her-home' },
						{ label: 'Talking to her', slug: 'docs/meet/talking-to-her' },
						{ label: 'Teaching her', slug: 'docs/meet/teaching-her' },
						{ label: 'Keeping her safe', slug: 'docs/meet/keeping-her-safe' },
						{ label: "When something's wrong", slug: 'docs/meet/when-somethings-wrong' },
						{ label: 'Glossary', slug: 'docs/meet/glossary' },
					],
				},
				{
					label: 'Under the hood',
					items: [
						{ label: 'How it fits together', slug: 'docs/under-the-hood/architecture' },
						{ label: 'Chat clients', slug: 'docs/under-the-hood/chat-clients' },
						{
							label: 'Skills, profiles, and jobs',
							slug: 'docs/under-the-hood/skills-profiles-and-jobs',
						},
						{ label: 'Waffle Desk', slug: 'docs/under-the-hood/waffle-desk' },
						{ label: 'The sandbox queue', slug: 'docs/under-the-hood/sandbox' },
						{ label: 'Code intelligence', slug: 'docs/under-the-hood/code-intelligence' },
						{ label: 'Deploying and running', slug: 'docs/under-the-hood/deployment' },
					],
				},
				{
					label: 'Reference',
					items: [
						{ label: 'Command reference', slug: 'docs/reference/cli' },
						{ label: 'Configuration reference', slug: 'docs/reference/configuration' },
					],
				},
			],
		}),
	],
	vite: {
		plugins: [tailwindcss()]
	}
});
