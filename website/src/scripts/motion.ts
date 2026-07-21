import gsap from 'gsap';
import { ScrollTrigger } from 'gsap/ScrollTrigger';

/** Strong ease-out (Emil / gsap-plugins craft) */
const EASE_OUT = 'power3.out';

function prefersReducedMotion(): boolean {
	return window.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

function initHeroTimeline(): void {
	const items = gsap.utils.toArray<HTMLElement>('[data-hero-anim]');
	if (!items.length) return;

	// Start slightly visible in scale terms — never scale(0)
	gsap.set(items, { opacity: 0, y: 14 });

	gsap
		.timeline({
			defaults: { ease: EASE_OUT, duration: 0.5 },
		})
		.to(items, {
			opacity: 1,
			y: 0,
			stagger: 0.07,
		});

	// One-shot light settle only — no infinite loop (Emil frequency)
	const light = document.querySelector<HTMLElement>('[data-hero-light]');
	if (light) {
		gsap.fromTo(
			light,
			{ opacity: 0.5, xPercent: -2 },
			{
				opacity: 1,
				xPercent: 0,
				duration: 1.1,
				ease: 'power2.out',
			},
		);
	}
}

function initScrollReveals(): void {
	const blocks = gsap.utils.toArray<HTMLElement>('[data-scroll-anim]');
	if (!blocks.length) return;

	blocks.forEach((el) => {
		gsap.fromTo(
			el,
			{ opacity: 0, y: 14 },
			{
				opacity: 1,
				y: 0,
				duration: 0.5,
				ease: EASE_OUT,
				scrollTrigger: {
					trigger: el,
					start: 'top 90%',
					once: true,
				},
			},
		);
	});
}

/**
 * Sticky crimes portrait: body stays put; only the clipped head layer tips
 * as the list scrolls past (ScrollTrigger scrub).
 */
function initStoryCatWatch(): void {
	const section = document.querySelector<HTMLElement>('[data-story-section]');
	const head = document.querySelector<HTMLElement>('[data-story-cat-head]');
	if (!section || !head) return;

	// Neck pivot: bottom-center of the head clip window
	gsap.set(head, { transformOrigin: '50% 100%' });

	gsap.fromTo(
		head,
		{ rotate: -5.5 },
		{
			rotate: 6.5,
			ease: 'none',
			scrollTrigger: {
				trigger: section,
				start: 'top 70%',
				end: 'bottom 35%',
				scrub: 0.65,
				invalidateOnRefresh: true,
			},
		},
	);
}

export function initMotion(): void {
	const root = document.documentElement;

	if (prefersReducedMotion()) {
		root.classList.add('js-motion-reduced');
		root.classList.add('js-motion-ready');
		return;
	}

	root.classList.add('js-motion');
	gsap.registerPlugin(ScrollTrigger);
	initHeroTimeline();
	initScrollReveals();
	initStoryCatWatch();

	window.addEventListener(
		'load',
		() => {
			ScrollTrigger.refresh();
		},
		{ once: true },
	);

	root.classList.add('js-motion-ready');
}

initMotion();
