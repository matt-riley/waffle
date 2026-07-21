# Skill Metadata Context-Budget Design

## Goal

Reduce the initial context cost of the 19 repository-local skills while preserving accurate implicit invocation.

## Scope

Change only the `description` field in each `.codex/skills/*/SKILL.md` frontmatter block. Keep skill names, bodies, licenses, file locations, and invocation policy unchanged. Do not add `agents/openai.yaml` files or disable implicit invocation.

## Metadata Contract

Every rewritten description must:

- start with `Use when`;
- describe triggering conditions rather than the skill's workflow or output;
- be no longer than 200 characters;
- retain the concrete terms needed to distinguish the skill from nearby skills;
- avoid promotional language, implementation summaries, and generic claims of quality;
- remain valid single-line YAML.

The aggregate description text should be less than half of the current 5,875-character baseline.

## Trigger Boundaries

The broad visual-design skills must identify distinct situations: new visual direction, redesigning an existing interface, detailed UI polish, responsive layout work, interaction behavior, or Waffle-specific personal copy. The hero and image-generation skills must remain limited to hero sections and generated website reference imagery respectively.

The GSAP skills must retain their distinguishing API or environment terms: core tweens, timelines, ScrollTrigger, plugins, utilities, performance, and Vue/Svelte lifecycle integration. The general GSAP skill may cover end-to-end GSAP implementation and debugging but must not absorb every specialist trigger.

## Validation

Validation will enforce the metadata contract before accepting the rewrite:

1. Establish a failing baseline against the current descriptions.
2. Rewrite the 19 descriptions only.
3. Run the contract check again and require all descriptions to pass.
4. Run the skill creator's `quick_validate.py` against every skill directory.
5. Confirm the aggregate character count is below 2,938 characters.
6. Inspect the final diff to ensure no skill body or unrelated file changed.

## Expected Result

Codex will receive materially smaller, front-loaded trigger metadata in its initial skills list. All skills will remain available for implicit and explicit invocation, while overlapping skills will be easier to select correctly.
