/**
 * Resolve the public site origin for the build.
 *
 * Extracted from astro.config.mjs so it stays directly testable: the config
 * itself imports Starlight, whose TypeScript entry point plain Node cannot
 * load from node_modules.
 *
 * A blank or whitespace-only value is treated as unset, so an empty
 * PUBLIC_SITE_URL in CI does not produce a site origin of "".
 */
export function resolveSiteURL(value) {
	return value?.trim() || undefined;
}
