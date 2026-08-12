# Changelog

## [0.17.0](https://github.com/matt-riley/waffle/compare/v0.16.0...v0.17.0) (2026-08-12)


### Features

* **#253:** notify tool for mid-run owner messages ([#374](https://github.com/matt-riley/waffle/issues/374)) ([72c0fed](https://github.com/matt-riley/waffle/commit/72c0fed0067eb26e88dbb04c2fb8ec4d99b5d8d2))
* **#256:** add ranged reads, file listing, and atomic batch edits ([#372](https://github.com/matt-riley/waffle/issues/372)) ([2ef9943](https://github.com/matt-riley/waffle/commit/2ef99437d84be08d5fe26251bb38a158f572e9f7))

## [0.16.0](https://github.com/matt-riley/waffle/compare/v0.15.0...v0.16.0) (2026-08-12)


### Features

* **#254:** scoped credentialed API faces for third-party APIs ([#377](https://github.com/matt-riley/waffle/issues/377)) ([8640886](https://github.com/matt-riley/waffle/commit/8640886f6f2a6d53e8e2a718ca80d0ae796213ef))

## [0.15.0](https://github.com/matt-riley/waffle/compare/v0.14.0...v0.15.0) (2026-08-12)


### Features

* **mcp:** streamable HTTP transport with OAuth and brokered egress ([#249](https://github.com/matt-riley/waffle/issues/249)) ([#378](https://github.com/matt-riley/waffle/issues/378)) ([58bac40](https://github.com/matt-riley/waffle/commit/58bac406de35fa75750c856098b722ed48d72f7f))

## [0.14.0](https://github.com/matt-riley/waffle/compare/v0.13.0...v0.14.0) (2026-08-12)


### Features

* **#247:** prompt caching breakpoints and cached-token accounting ([#375](https://github.com/matt-riley/waffle/issues/375)) ([6f806df](https://github.com/matt-riley/waffle/commit/6f806df63a3ef6d5a19dafdd82ea8442f03ad8e9))

## [0.13.0](https://github.com/matt-riley/waffle/compare/v0.12.8...v0.13.0) (2026-08-12)


### Features

* **#248:** fetch shapes HTML, JSON and binary bodies by content type ([#369](https://github.com/matt-riley/waffle/issues/369)) ([baab341](https://github.com/matt-riley/waffle/commit/baab341b7a1098e8ffa9f67320ae51b2236bf6b1))
* **#250:** add image and document blocks to canonical LLM types ([#373](https://github.com/matt-riley/waffle/issues/373)) ([9158dc8](https://github.com/matt-riley/waffle/commit/9158dc878df7511c9856ddceaa48f5cfb7a4d4a0))
* **#252:** host-side GitHub tools mirroring github_pr ([#370](https://github.com/matt-riley/waffle/issues/370)) ([4995a35](https://github.com/matt-riley/waffle/commit/4995a35ec52535b63fa9bc91ffc0f3f652a4c0c4))
* **channel:** message attachments with fetch/send capability interfaces ([#251](https://github.com/matt-riley/waffle/issues/251)) ([#371](https://github.com/matt-riley/waffle/issues/371)) ([f495da8](https://github.com/matt-riley/waffle/commit/f495da877a70f3f8fcf0ed57567f2b6b21818e75))


### Bug Fixes

* **#255:** close codeintel honesty acceptance criteria ([#368](https://github.com/matt-riley/waffle/issues/368)) ([fd00967](https://github.com/matt-riley/waffle/commit/fd0096706cf72080e25c95d9b526450eac4baec2))

## [0.12.8](https://github.com/matt-riley/waffle/compare/v0.12.7...v0.12.8) (2026-08-12)


### Bug Fixes

* **agent:** bound summary cache ([#361](https://github.com/matt-riley/waffle/issues/361)) ([20708b0](https://github.com/matt-riley/waffle/commit/20708b021dd1f4c8813b08e6d1644e2132870958))
* **memory:** widen note IDs and index collision checks ([#363](https://github.com/matt-riley/waffle/issues/363)) ([2e4e2ff](https://github.com/matt-riley/waffle/commit/2e4e2ffb5b637be27c0b86e14474d18eff71fc0a))
* **schedule:** fire in-process retry timers at the persisted deadline ([#367](https://github.com/matt-riley/waffle/issues/367)) ([5997690](https://github.com/matt-riley/waffle/commit/59976904eea3a9834d7d56a4d1b78771d13db804))
* **session:** chunk ExistIDs queries ([#364](https://github.com/matt-riley/waffle/issues/364)) ([a95ff38](https://github.com/matt-riley/waffle/commit/a95ff389b44d69e512ffd08f4807bbf544d5d081))
* **tool:** bound write and edit file content ([#266](https://github.com/matt-riley/waffle/issues/266)) ([#362](https://github.com/matt-riley/waffle/issues/362)) ([a3f502d](https://github.com/matt-riley/waffle/commit/a3f502dcd65aa8508cc47fd57773b663134ea0b8))
* **tool:** limit host bash process trees ([#365](https://github.com/matt-riley/waffle/issues/365)) ([3be3e5a](https://github.com/matt-riley/waffle/commit/3be3e5acc3a229315f7a38d5c07ccf97b9389a36))

## [0.12.7](https://github.com/matt-riley/waffle/compare/v0.12.6...v0.12.7) (2026-08-09)


### Bug Fixes

* **#278:** reuse one http.Transport across fetch calls ([#353](https://github.com/matt-riley/waffle/issues/353)) ([660631d](https://github.com/matt-riley/waffle/commit/660631d2db9bc08bd2c1c3dd4052edc7eed5bbb2))
* **#279:** truncate on UTF-8 rune boundaries in gitcred ([#349](https://github.com/matt-riley/waffle/issues/349)) ([b20f58e](https://github.com/matt-riley/waffle/commit/b20f58efcf802ff9d1d9716e9d5e985fa9b7920d))
* **#280:** never slice tool-output spills mid-rune ([#350](https://github.com/matt-riley/waffle/issues/350)) ([77d6bd3](https://github.com/matt-riley/waffle/commit/77d6bd36d58bcba416de6579b0ec267cb50ea504))
* **#281:** reject ambiguous chat-only profile references ([#352](https://github.com/matt-riley/waffle/issues/352)) ([09d6f31](https://github.com/matt-riley/waffle/commit/09d6f31662dffff98eeb33ea7584ff729095153b))
* **#282:** scope repo policy to each open, never the shared Manager ([#357](https://github.com/matt-riley/waffle/issues/357)) ([01b5a67](https://github.com/matt-riley/waffle/commit/01b5a675c9fde7a19059f4a5aa631097f315e49d))
* **#283:** clean up volume/session when devcontainer adoption fails ([#354](https://github.com/matt-riley/waffle/issues/354)) ([147ff22](https://github.com/matt-riley/waffle/commit/147ff22d156fad30febc0d36409406dbc9ff1be4))
* **#284:** fail runs when AppendTurn persistence fails ([#355](https://github.com/matt-riley/waffle/issues/355)) ([488bf8a](https://github.com/matt-riley/waffle/commit/488bf8ac07a5fbc52615b99074d664beb2b35b33))
* **#285:** implement RunWithID on DockerExecutor ([#356](https://github.com/matt-riley/waffle/issues/356)) ([e029837](https://github.com/matt-riley/waffle/commit/e0298379affdabae485589bcd60285ad02e2bcce))

## [0.12.6](https://github.com/matt-riley/waffle/compare/v0.12.5...v0.12.6) (2026-08-09)


### Bug Fixes

* **#290:** make GroupFor session+group creation atomic ([#343](https://github.com/matt-riley/waffle/issues/343)) ([08047ec](https://github.com/matt-riley/waffle/commit/08047ecc0d96371e322dcb8c5c7bed5395598f5f))
* **#292:** fail closed on usage accounting write failures ([#342](https://github.com/matt-riley/waffle/issues/342)) ([2a15617](https://github.com/matt-riley/waffle/commit/2a15617a09f46974f0675e200e336c1c72649861))
* **#295:** stage only proposal paths in accepted learning commits ([#341](https://github.com/matt-riley/waffle/issues/341)) ([8035948](https://github.com/matt-riley/waffle/commit/8035948247766959d879e5414e85ed4dfeb02f57))
* **#296:** record workspace/session on running intake claims ([#340](https://github.com/matt-riley/waffle/issues/340)) ([9a9e4d0](https://github.com/matt-riley/waffle/commit/9a9e4d087ab666a8ee64d1afc4b906ab60b35387))
* **#298:** propagate subagent handoff persistence failures ([#339](https://github.com/matt-riley/waffle/issues/339)) ([2fe3875](https://github.com/matt-riley/waffle/commit/2fe3875f13eeba06a9c4749a21332381839b8a7d))
* **#299:** treat cron attempt/outcome/retry persistence as part of firing ([#338](https://github.com/matt-riley/waffle/issues/338)) ([060ed57](https://github.com/matt-riley/waffle/commit/060ed574ddb6370ca62b55faa5e220bfce1288b9))

## [0.12.5](https://github.com/matt-riley/waffle/compare/v0.12.4...v0.12.5) (2026-08-09)


### Performance Improvements

* **ci:** parallelize artifact build and make inspection probe injectable ([#335](https://github.com/matt-riley/waffle/issues/335)) ([#336](https://github.com/matt-riley/waffle/issues/336)) ([a61614b](https://github.com/matt-riley/waffle/commit/a61614b525fab7985b3bb86b8a854f3ffb035d66))

## [0.12.4](https://github.com/matt-riley/waffle/compare/v0.12.3...v0.12.4) (2026-08-08)


### Bug Fixes

* **#261:** never leave a run pinned active when its metrics are lost ([#330](https://github.com/matt-riley/waffle/issues/330)) ([de4499c](https://github.com/matt-riley/waffle/commit/de4499cd0a2c8d49298e7bafc5ca11b7fc5e8e95))
* **#263:** fsync the secret store and backup writes before publishing them ([#331](https://github.com/matt-riley/waffle/issues/331)) ([48ab125](https://github.com/matt-riley/waffle/commit/48ab125128bd7c77ac1ec0153f733aca1e72143b))
* **#267:** serialize MEMORY.md mutation across processes ([#333](https://github.com/matt-riley/waffle/issues/333)) ([e637271](https://github.com/matt-riley/waffle/commit/e6372716101175740bfbead2453ee256bc650b1e))
* **#270:** persist final turns on a bounded detached context ([#334](https://github.com/matt-riley/waffle/issues/334)) ([cbc896c](https://github.com/matt-riley/waffle/commit/cbc896c0d2dbc3924c9f26eb1ff058ae587bbea1))

## [0.12.3](https://github.com/matt-riley/waffle/compare/v0.12.2...v0.12.3) (2026-08-08)


### Bug Fixes

* **#257:** make Telegram delivery durable and ack-gated ([#326](https://github.com/matt-riley/waffle/issues/326)) ([ec77a04](https://github.com/matt-riley/waffle/commit/ec77a041b86ce8d1f409ad551b95974a39bef6f1))
* **#259:** retry the startup memory FTS reindex after a failure ([#327](https://github.com/matt-riley/waffle/issues/327)) ([601d22a](https://github.com/matt-riley/waffle/commit/601d22a7288739ebf7c676d4778e5eee28003d03))
* **#260:** report workspace activity write failures and corroborate idleness ([#329](https://github.com/matt-riley/waffle/issues/329)) ([edc9aca](https://github.com/matt-riley/waffle/commit/edc9aca3d711ab7caf4ca7ec0b1fb22d85216fbb))
* **#291:** isolate tool panics from the serve process ([#324](https://github.com/matt-riley/waffle/issues/324)) ([f3a7573](https://github.com/matt-riley/waffle/commit/f3a75734fc70fab0922774139a342c1ed6a200c4))
* **#297:** report lost policy_audit writes instead of discarding them ([#325](https://github.com/matt-riley/waffle/issues/325)) ([166a5a4](https://github.com/matt-riley/waffle/commit/166a5a4f94262ee6e43ea4a08782911d6d1989d9))

## [0.12.2](https://github.com/matt-riley/waffle/compare/v0.12.1...v0.12.2) (2026-08-08)


### Bug Fixes

* **#239:** keep workspace git host reachable under allowlist ([#322](https://github.com/matt-riley/waffle/issues/322)) ([c5549b7](https://github.com/matt-riley/waffle/commit/c5549b742d16c69dca594f6d49e42eeb20f98394))

## [0.12.1](https://github.com/matt-riley/waffle/compare/v0.12.0...v0.12.1) (2026-08-08)


### Bug Fixes

* **codeintel:** report unsupported language limitations ([#317](https://github.com/matt-riley/waffle/issues/317)) ([192f506](https://github.com/matt-riley/waffle/commit/192f506b71771d7d086319058ed2f2f6937e3f7a))

## [0.12.0](https://github.com/matt-riley/waffle/compare/v0.11.0...v0.12.0) (2026-08-08)


### Features

* **tool:** confine the builtin file tools to configured roots ([#302](https://github.com/matt-riley/waffle/issues/302)) ([fce8556](https://github.com/matt-riley/waffle/commit/fce85568ad7003f2f6a15f6578ba3ff8772baf01)), closes [#269](https://github.com/matt-riley/waffle/issues/269)


### Bug Fixes

* **ci:** resolve grpc vulnerability and desk serve test flake ([#318](https://github.com/matt-riley/waffle/issues/318)) ([a3b3ac5](https://github.com/matt-riley/waffle/commit/a3b3ac5916f0dacf511a8a2fc02a8a2ed7c2b8bd))
* **mcp:** cap the stdio transport and tool results ([#300](https://github.com/matt-riley/waffle/issues/300)) ([35ff807](https://github.com/matt-riley/waffle/commit/35ff8077efeb6e634a3c89a7a983dbb85b608e6a)), closes [#286](https://github.com/matt-riley/waffle/issues/286) [#265](https://github.com/matt-riley/waffle/issues/265)
* **tool:** write files atomically instead of truncating in place ([#301](https://github.com/matt-riley/waffle/issues/301)) ([816e7b8](https://github.com/matt-riley/waffle/commit/816e7b8478c9ce84ef60a5320d40e422ee36e929))

## [0.11.0](https://github.com/matt-riley/waffle/compare/v0.10.0...v0.11.0) (2026-07-26)


### Features

* **broker:** tunnel HTTPS egress with CONNECT ([#236](https://github.com/matt-riley/waffle/issues/236)) ([a94d7ad](https://github.com/matt-riley/waffle/commit/a94d7ad2e15eca1ddc951fe824d98910cf0a615a))
* **github:** open pull requests from a host-side tool ([#229](https://github.com/matt-riley/waffle/issues/229)) ([4509aa2](https://github.com/matt-riley/waffle/commit/4509aa2e6b4cf123e456e3ccdf6e82962c2c0167))


### Bug Fixes

* **serve:** give /repo workspaces the egress proxy ([#233](https://github.com/matt-riley/waffle/issues/233)) ([acd80d6](https://github.com/matt-riley/waffle/commit/acd80d6b47f349019e113eb686f0b0fa350e9a8f))
* **serve:** let /repo reuse the broker serve already runs ([#232](https://github.com/matt-riley/waffle/issues/232)) ([e78ce12](https://github.com/matt-riley/waffle/commit/e78ce12a739a8db0bf845e340c2c4dc0ce0b4024))
* **serve:** log why a chat command failed ([#235](https://github.com/matt-riley/waffle/issues/235)) ([fbfdd04](https://github.com/matt-riley/waffle/commit/fbfdd0402426cd693e22221c95b596e5e08fd0a0))
* **workspace:** send the repo path to the credential helper ([#237](https://github.com/matt-riley/waffle/issues/237)) ([8936123](https://github.com/matt-riley/waffle/commit/8936123966274cb36b791482580193f20772738e))
* **workspace:** stop git asking the broker for a proxy password ([#234](https://github.com/matt-riley/waffle/issues/234)) ([f10a225](https://github.com/matt-riley/waffle/commit/f10a2253587deb14268b71aa609b119ecc1f006d))

## [0.10.0](https://github.com/matt-riley/waffle/compare/v0.9.2...v0.10.0) (2026-07-26)


### Features

* **#195:** adopt htmx fragments for Desk sections ([#226](https://github.com/matt-riley/waffle/issues/226)) ([e2dea83](https://github.com/matt-riley/waffle/commit/e2dea83e96327d6497d9413b333bd335e5eb88b1))
* **desk:** add a structured agent-profile editor ([#223](https://github.com/matt-riley/waffle/issues/223)) ([a740014](https://github.com/matt-riley/waffle/commit/a74001424a040c9fcfa67cd5ffbadd3f8833b919))
* **desk:** complete capabilities models and providers ([#219](https://github.com/matt-riley/waffle/issues/219)) ([86d3eb1](https://github.com/matt-riley/waffle/commit/86d3eb1143faa3244fadc2cc4b7442f7757e6b5f))
* **desk:** complete skill review and lifecycle controls ([#222](https://github.com/matt-riley/waffle/issues/222)) ([8c13096](https://github.com/matt-riley/waffle/commit/8c13096f63ef23868409157f983b2ffdebe238cd))
* **desk:** expand Today commands and recovery ([#217](https://github.com/matt-riley/waffle/issues/217)) ([9d1bdbd](https://github.com/matt-riley/waffle/commit/9d1bdbd4db5eb26f7acc5cbbd70d404d6f20ff5b))
* **desk:** report and satisfy setup prerequisites ([#224](https://github.com/matt-riley/waffle/issues/224)) ([83873ce](https://github.com/matt-riley/waffle/commit/83873ce298030820fccf7a52d103f3f81068fd16))
* **desk:** show the system prompt and layered tool policy ([#221](https://github.com/matt-riley/waffle/issues/221)) ([94dbc01](https://github.com/matt-riley/waffle/commit/94dbc012e4743d4de93b5e77d62f30d1012f1aa2))
* **desk:** surface workspace git state and GitHub connections ([#220](https://github.com/matt-riley/waffle/issues/220)) ([6806578](https://github.com/matt-riley/waffle/commit/68065789cb8cff518a76cfbad167f0b17fd0a0a8))


### Bug Fixes

* **#188:** make capability removals durable ([#225](https://github.com/matt-riley/waffle/issues/225)) ([cb152ac](https://github.com/matt-riley/waffle/commit/cb152acc1662fd6f28c818aaed5fb430907cf562))

## [0.9.2](https://github.com/matt-riley/waffle/compare/v0.9.1...v0.9.2) (2026-07-25)


### Bug Fixes

* **provider:** stop holding the provider lock across the readiness probe ([#214](https://github.com/matt-riley/waffle/issues/214)) ([5789d69](https://github.com/matt-riley/waffle/commit/5789d695c8a76bfe5ab78894f1ad64c82462cbfd))

## [0.9.1](https://github.com/matt-riley/waffle/compare/v0.9.0...v0.9.1) (2026-07-25)


### Bug Fixes

* **desk:** drive rail connection and model from live state ([#208](https://github.com/matt-riley/waffle/issues/208)) ([1f4cdae](https://github.com/matt-riley/waffle/commit/1f4cdaedb4a4277cfad59000441a63f6e0845c23))
* **desk:** include inputs in shared control baseline ([#207](https://github.com/matt-riley/waffle/issues/207)) ([344e912](https://github.com/matt-riley/waffle/commit/344e9122842870331e70f90b0d67b87f19647836))
* **desk:** isolate capability form status and pending state ([#210](https://github.com/matt-riley/waffle/issues/210)) ([beeea8b](https://github.com/matt-riley/waffle/commit/beeea8be819f780588bba4e01bf37c0235b7e115))
* **desk:** keep Today usable on recoverable failures ([#209](https://github.com/matt-riley/waffle/issues/209)) ([a93dac3](https://github.com/matt-riley/waffle/commit/a93dac30fd7987f1a826e9cdb3097e8c04905336))
* **desk:** map capability and workspace failures to stable codes ([#206](https://github.com/matt-riley/waffle/issues/206)) ([c9cb392](https://github.com/matt-riley/waffle/commit/c9cb392a90153af0f3e4e6182a038c44e3fb3fe9))
* **provider:** re-prove readiness at startup instead of stranding Installed ([#211](https://github.com/matt-riley/waffle/issues/211)) ([0c325f9](https://github.com/matt-riley/waffle/commit/0c325f925c3467f45bc06a3567711cbca06dc789))
* **workspace:** survive queue contention when probing the inspection heartbeat ([#213](https://github.com/matt-riley/waffle/issues/213)) ([feb97bd](https://github.com/matt-riley/waffle/commit/feb97bd4bcda7bf3145c3457fb7ea388544351dc))

## [0.9.0](https://github.com/matt-riley/waffle/compare/v0.8.5...v0.9.0) (2026-07-25)


### Features

* **dashboard:** admit tailnet Desk requests via Tailscale identity ([#205](https://github.com/matt-riley/waffle/issues/205)) ([124e74d](https://github.com/matt-riley/waffle/commit/124e74d08038420e0f3170084c2bfe6dff697d2e))


### Bug Fixes

* **broker:** expire session tokens after TTL ([#198](https://github.com/matt-riley/waffle/issues/198)) ([#202](https://github.com/matt-riley/waffle/issues/202)) ([5132df3](https://github.com/matt-riley/waffle/commit/5132df3e99a24ddde13946802d35ba7b57d1119e))
* **desk:** surface restart outcome instead of infinite poll ([#203](https://github.com/matt-riley/waffle/issues/203)) ([7747ce8](https://github.com/matt-riley/waffle/commit/7747ce87c1de6b143a1dcb3535360f87f63e647a))
* **skill:** fail closed when skill_status cannot be read ([#201](https://github.com/matt-riley/waffle/issues/201)) ([87e39af](https://github.com/matt-riley/waffle/commit/87e39afcdf4c4057fd68af9e2815161738533f6a))

## [0.8.5](https://github.com/matt-riley/waffle/compare/v0.8.4...v0.8.5) (2026-07-25)


### Bug Fixes

* **schedule:** recheck enabled state after a skipped retry-wait loop ([#199](https://github.com/matt-riley/waffle/issues/199)) ([ed253a6](https://github.com/matt-riley/waffle/commit/ed253a6a880f544b6632b72cf881ac253e1c8908)), closes [#196](https://github.com/matt-riley/waffle/issues/196)

## [0.8.4](https://github.com/matt-riley/waffle/compare/v0.8.3...v0.8.4) (2026-07-24)


### Bug Fixes

* address Greptile review comments from [#165](https://github.com/matt-riley/waffle/issues/165) [#166](https://github.com/matt-riley/waffle/issues/166) [#168](https://github.com/matt-riley/waffle/issues/168) ([#170](https://github.com/matt-riley/waffle/issues/170)) ([a9fedad](https://github.com/matt-riley/waffle/commit/a9fedad656ec3a27f884ead94d4f8bf86a4b18f7))
* address Greptile review comments on ttlmap ([#167](https://github.com/matt-riley/waffle/issues/167)) ([#172](https://github.com/matt-riley/waffle/issues/172)) ([93787cf](https://github.com/matt-riley/waffle/commit/93787cff43ee2c39b5cace13ac88c364ff6279ba))

## [0.8.3](https://github.com/matt-riley/waffle/compare/v0.8.2...v0.8.3) (2026-07-24)


### Bug Fixes

* **#151:** cache Desk static assets at process start ([#165](https://github.com/matt-riley/waffle/issues/165)) ([30cb33e](https://github.com/matt-riley/waffle/commit/30cb33ed185619a169d3caa6f1b301129b55f565))
* **#152:** audit desk and skillinstall mutations in policy_audit ([#166](https://github.com/matt-riley/waffle/issues/166)) ([21a7511](https://github.com/matt-riley/waffle/commit/21a7511c96f889a33ca30920548c02b59c55d526))
* **#154:** unify Desk bounded/TTL token stores ([#167](https://github.com/matt-riley/waffle/issues/167)) ([9088983](https://github.com/matt-riley/waffle/commit/9088983c04f867cf555258c5077d97d67600dec6))
* **#156:** share crash-safe file commit for skillinstall provenance ([#168](https://github.com/matt-riley/waffle/issues/168)) ([f9e4a61](https://github.com/matt-riley/waffle/commit/f9e4a613f34f640e0a03fd279ab166098300ca3f))

## [0.8.2](https://github.com/matt-riley/waffle/compare/v0.8.1...v0.8.2) (2026-07-24)


### Bug Fixes

* **#148:** start idle workspace once during clean close ([#160](https://github.com/matt-riley/waffle/issues/160)) ([4ca9145](https://github.com/matt-riley/waffle/commit/4ca914595d8aff9e1c786c7f04a56e019754000a))
* **#150:** batch tasks session existence for OpenAtDesk ([#162](https://github.com/matt-riley/waffle/issues/162)) ([f211dd8](https://github.com/matt-riley/waffle/commit/f211dd84cd22c33ecd3856df310d5f9eb21150c1))

## [0.8.1](https://github.com/matt-riley/waffle/compare/v0.8.0...v0.8.1) (2026-07-24)


### Bug Fixes

* **#149:** discover skills once for capabilities list ([#163](https://github.com/matt-riley/waffle/issues/163)) ([6033e13](https://github.com/matt-riley/waffle/commit/6033e13ae30094be2aa6c1e7f3adca38ac07bc5d)), closes [#149](https://github.com/matt-riley/waffle/issues/149)
* **#153:** project chat secrets by structure and exact redaction ([#159](https://github.com/matt-riley/waffle/issues/159)) ([7a5138e](https://github.com/matt-riley/waffle/commit/7a5138e8a7aafa5fa3fe0634707b851c75a92ede))
* **#155:** resolve profile sandbox from own group, not main ([#157](https://github.com/matt-riley/waffle/issues/157)) ([728db63](https://github.com/matt-riley/waffle/commit/728db63c9d8f43ce4dbb21b0b26da521b1861e95))
* isolate dashboard serve test from managed host state ([bbb2d32](https://github.com/matt-riley/waffle/commit/bbb2d32f886e47a6f9489b17eccaa8ba30c81b9d))

## [0.8.0](https://github.com/matt-riley/waffle/compare/v0.7.0...v0.8.0) (2026-07-24)


### Features

* add embedded Waffle Desk personal cockpit ([#146](https://github.com/matt-riley/waffle/issues/146)) ([6461df0](https://github.com/matt-riley/waffle/commit/6461df0a3df39d2471fa2ce2f0b7f6ccb4d678ea))

## [0.7.0](https://github.com/matt-riley/waffle/compare/v0.6.0...v0.7.0) (2026-07-21)


### Features

* add Waffle project website ([#139](https://github.com/matt-riley/waffle/issues/139)) ([ebdef40](https://github.com/matt-riley/waffle/commit/ebdef40abfb2d0fcdb2a88ef66f009763714e50d))


### Bug Fixes

* kill bash tool process group on timeout ([177fc08](https://github.com/matt-riley/waffle/commit/177fc0880197540da48ad31fd98f84c836578424))
* kill bash tool process group on timeout ([97aaa54](https://github.com/matt-riley/waffle/commit/97aaa54df87baaa5a7b3b25852d35e0c37ff8302))
* tie MCP child process lifetime to client, not handshake context ([849bf2b](https://github.com/matt-riley/waffle/commit/849bf2bf6d52b34910d31cacaf0003e1e09e6017))

## [0.6.0](https://github.com/matt-riley/waffle/compare/v0.5.0...v0.6.0) (2026-07-21)


### Features

* **#130:** add prompt history traversal with Up/Down ([c9a8498](https://github.com/matt-riley/waffle/commit/c9a8498a68ec44b8270d5d3ae09c7c63944cd984))
* **#132:** add activity spinner during active turns ([69ef685](https://github.com/matt-riley/waffle/commit/69ef685154e7b80069c970cca027b848a607032b))
* **#133:** add --config/-c and WAFFLE_CONFIG override ([60cbca2](https://github.com/matt-riley/waffle/commit/60cbca2539f13eb26b13ad53da02bcfa7ec18dda))
* **#134:** add --json flag to listing subcommands ([7182741](https://github.com/matt-riley/waffle/commit/71827413355812645c8f996a4d5a27a43d6af08e))
* **#136:** add shell completion for bash zsh fish ([35f8a51](https://github.com/matt-riley/waffle/commit/35f8a5127e3cf5554bc6ffdc4d8b69d8f361a6b0))


### Bug Fixes

* **#118:** remove redundant min/max helpers ([26ca0ad](https://github.com/matt-riley/waffle/commit/26ca0ad2701237787956c857bc8d61a44c4b5cc6))
* **#126:** prevent data loss on forget --help ([c47481d](https://github.com/matt-riley/waffle/commit/c47481d0e91ffcbc9b093f400e167b0292f42089))
* **#127:** prevent daemon launch on serve --help ([1978a7a](https://github.com/matt-riley/waffle/commit/1978a7ae50f311b4a292ccc9a52a9e2d7644cc8d))
* **#128:** prevent git checkout on upgrade --help ([eb49927](https://github.com/matt-riley/waffle/commit/eb49927871fd2bfe34813b88e50af551f261d9a5))
* **#129:** support single-string cron and --deliver= ([6ddf758](https://github.com/matt-riley/waffle/commit/6ddf75821d76b572f7e6e3ac652d47708f66c486))
* **#131:** use AST-based markdown renderer ([66fc684](https://github.com/matt-riley/waffle/commit/66fc684844e0c7161aea2ffb0e3a86050d43ffef))

## [0.5.0](https://github.com/matt-riley/waffle/compare/v0.4.0...v0.5.0) (2026-07-21)


### Features

* **#123:** live elapsed and tokens in busy chat footer ([e7cdfc3](https://github.com/matt-riley/waffle/commit/e7cdfc3cc16bf3456713bbd297467ef19317c25c))
* **#124:** add waffle setup first-run command ([07ebd59](https://github.com/matt-riley/waffle/commit/07ebd5976dcb32cb2ea4454f254c461f5c43384e))
* **#125:** show command descriptions in slash palette ([0451e54](https://github.com/matt-riley/waffle/commit/0451e54566003c2d4b66d2c4a88c6202d56ea0df))


### Bug Fixes

* **#100:** close Anthropic stream after Complete ([961e4d6](https://github.com/matt-riley/waffle/commit/961e4d68bd2c399f18bba8a260eeba563b99fd49))
* **#101:** wait for intake dispatch goroutines on shutdown ([d018c1d](https://github.com/matt-riley/waffle/commit/d018c1d87f769bb2dde0614ffe684ed7ba6161c1))
* **#102:** fail closed on unverifiable child regex allows in Narrow ([503ddbd](https://github.com/matt-riley/waffle/commit/503ddbd77d5d2b3e2ede145539f0c24af7349300))
* **#103:** serialize MEMORY.md mutations across tools ([a99003a](https://github.com/matt-riley/waffle/commit/a99003a01e9bed36c3445db2133677e8c2b56afc))
* **#104:** make workset Add/Replace cap checks transactional ([bb2bbc8](https://github.com/matt-riley/waffle/commit/bb2bbc83704f40b44559763491601b43f84f5f18))
* **#105:** reject policy rules with empty tool/match/regex ([0e1a20e](https://github.com/matt-riley/waffle/commit/0e1a20e67c4aa5f0544f67dbd14c96da6cc02887))
* **#106:** deliver sandbox session token via file, not docker -e ([d41a827](https://github.com/matt-riley/waffle/commit/d41a827c8d1d17ac773b17ae53cd456c9d5e1e02))
* **#107:** UTF-8-safe truncation for tools, subagents, gateway ([99a8233](https://github.com/matt-riley/waffle/commit/99a8233c97d8e127efc9e31f64e2cf7069f2ad8d))
* **#108:** fail closed when group-tier GroupProfiles is nil ([ffd7d8f](https://github.com/matt-riley/waffle/commit/ffd7d8f3df0b16fd8dab7f5b8c4c488096881e86))
* **#111:** throttle transcript re-render during text streaming ([19039e7](https://github.com/matt-riley/waffle/commit/19039e7a909ab821018038be9fd598ca72416a07))
* **#112:** remove modulo bias from NewPairingCode ([5d24c0a](https://github.com/matt-riley/waffle/commit/5d24c0ab55aa69d4f4f1d4f77e4cc8787e1f4b72))
* **#113:** warn on memory notes FTS Upsert failure ([a1bfa4a](https://github.com/matt-riley/waffle/commit/a1bfa4a6ddd7b86d0007e6632b15ec613d763c63))
* **#114:** skip config-dependent doctor checks after load failure ([8c39827](https://github.com/matt-riley/waffle/commit/8c398271611c003a7925aec2aef6a217220f0dae))
* **#115:** consistent nil-store handling in Finish/Snapshot ([f4afc31](https://github.com/matt-riley/waffle/commit/f4afc319a8a95e1a17336505a291e5aba29de4ae))
* **#116:** return ErrNotFound from SetTitle/SetSummary ([6564df9](https://github.com/matt-riley/waffle/commit/6564df90418eaff4b28a0d1c20f5731369320a11))
* **#117:** paginate GitHub ListOpen via Link headers ([ed79908](https://github.com/matt-riley/waffle/commit/ed79908ba4ee1a9b958b2067c962f3720f172798))
* **#119:** flock secret filestore across processes ([73c2ac6](https://github.com/matt-riley/waffle/commit/73c2ac6d8249e7238698c22538811019c0ccf510))
* **#120:** skip summary cache when session id empty ([b2369c2](https://github.com/matt-riley/waffle/commit/b2369c269d2a3f87e02b4dc634fe116c94f147ce))
* **#122:** queue or notice busy chat composer submit ([be0c26d](https://github.com/matt-riley/waffle/commit/be0c26de191d007d7d52168f6f799e46aea45843))
* **#95,#110:** host-reachable workspace network and reaper continue ([5ab9354](https://github.com/matt-riley/waffle/commit/5ab93547f1b1f9f611b501183507209c9cbb7941))
* **#95:** drop unused dockerRunProbe helper (lint) ([ab0031c](https://github.com/matt-riley/waffle/commit/ab0031c82732190b7a1ffcf88aed93f7db2749fd))
* **#95:** fail-closed netlock; Docker tests drive shipped lockdown ([77362d4](https://github.com/matt-riley/waffle/commit/77362d491ed38e0bc0ce5979261ec91c221992cc))
* **#95:** lock down workspace routes; allow repo host for clone ([d364584](https://github.com/matt-riley/waffle/commit/d364584adb8f6e01959e82e0d21f14f61ee62e26))
* **#96:** enforce parent usage limits on subagent runs ([dfe8c8f](https://github.com/matt-riley/waffle/commit/dfe8c8f7a0744cabc9a66226789ac7e75bad7a49))
* **#97:** stop docker MCP containers on Close ([b0a0b63](https://github.com/matt-riley/waffle/commit/b0a0b63763d9d6686872924713327f059d7a9aa6))
* **#98:** redact longest secret values first ([52ef31f](https://github.com/matt-riley/waffle/commit/52ef31f1978cab5e4a845eac792efe5a7dcd9ca9))
* **#99,#109:** bind broker synchronously and join on shutdown ([a2f8daf](https://github.com/matt-riley/waffle/commit/a2f8daf711694f5f6bbb47deced27ddf63160407))
* check Close returns in netlock lockdown ([#137](https://github.com/matt-riley/waffle/issues/137)) ([9dcec00](https://github.com/matt-riley/waffle/commit/9dcec00179e6744f7d6090c14eaaf33ac3ef3d91))

## [0.4.0](https://github.com/matt-riley/waffle/compare/v0.3.0...v0.4.0) (2026-07-21)


### Features

* add focused conversation chat TUI ([23acf23](https://github.com/matt-riley/waffle/commit/23acf233f28d20a7a1507be5094bcd68652303d0))
* add local chat wire protocol ([d5049fd](https://github.com/matt-riley/waffle/commit/d5049fd2f05d05623d626a5d3ff7daf6e586ed12))
* add stateful chat runtime commands ([ea3e503](https://github.com/matt-riley/waffle/commit/ea3e50331d23745d20fb4c6ae2aabe944e32d87c))
* define chat command contract ([b827e78](https://github.com/matt-riley/waffle/commit/b827e783af4ce1d607dfc547a2244ba4d744f33d))
* persist chat model per session ([fdb3bee](https://github.com/matt-riley/waffle/commit/fdb3bee78991818ee22dea070a921f277ba5d4af))
* serve chat over a local Unix socket ([b638cc4](https://github.com/matt-riley/waffle/commit/b638cc4a6722649d3053e47b451eef529fea0612))


### Bug Fixes

* bound chat runtime close ([ae18b68](https://github.com/matt-riley/waffle/commit/ae18b682ac972aaf6baffc179ca6d4c804aa8665))
* bound local chat wire shutdown ([3f3a30f](https://github.com/matt-riley/waffle/commit/3f3a30f050c3d9b44fa2e63df3b43e75c98978ba))
* clear command state before terminal response ([0d8ac07](https://github.com/matt-riley/waffle/commit/0d8ac07e89ec896c58b6cabf7cf9053b8649e2c6))
* clear stale plain chat confirmations ([63777a9](https://github.com/matt-riley/waffle/commit/63777a98392584471b252e6aa0524d8d907ab5e1))
* complete chat shutdown lifecycle ([a49d27c](https://github.com/matt-riley/waffle/commit/a49d27c639c26ce1f035ce9a5f1b927abef3a1df))
* complete focused conversation lifecycle ([61c6fe9](https://github.com/matt-riley/waffle/commit/61c6fe95de07e047cf3a59f9ce14e3909d9b4eea))
* document chat Enter key ([16f3025](https://github.com/matt-riley/waffle/commit/16f3025be5277625e93a3bd2ada58f62a0e79713))
* harden focused conversation TUI ([acf4851](https://github.com/matt-riley/waffle/commit/acf4851825671511674dfc8f6af32f7203443efb))
* harden local chat socket ownership ([f8c9e0e](https://github.com/matt-riley/waffle/commit/f8c9e0e6767075bccdb24b8e84b14e1ef3d49915))
* harden local chat wire lifecycle ([c4dc227](https://github.com/matt-riley/waffle/commit/c4dc2272de718e3e6078addabbc52156f3c46403))
* harden managed chat lifecycle ([4bbe050](https://github.com/matt-riley/waffle/commit/4bbe05099cc5bcefcdc9daa5b42d20574a7225a1))
* harden plain chat lifecycle ([2b260a3](https://github.com/matt-riley/waffle/commit/2b260a317ff052c4a0dcb9135279b6b6669187f3))
* isolate chat repo transitions ([e879e1f](https://github.com/matt-riley/waffle/commit/e879e1f5a1b4fb266361fc61ad8be00ca765cc2a))
* preserve chat profile across repo switches ([5117e1d](https://github.com/matt-riley/waffle/commit/5117e1d8752edd596653157854b9cfc3e37fe0f1))
* validate local socket ancestor chain ([a732b29](https://github.com/matt-riley/waffle/commit/a732b299916b37ba769f5c8271dedc4b71b567a9))

## [0.3.0](https://github.com/matt-riley/waffle/compare/v0.2.0...v0.3.0) (2026-07-20)


### Features

* add provider catalogue presets ([04d33ca](https://github.com/matt-riley/waffle/commit/04d33cae4bd11a69cc0e3bf37a0ca709d1397678))
* add provider model catalogue domain ([174ef45](https://github.com/matt-riley/waffle/commit/174ef45cb0db369aedd64d2d0e4304d19e2333c2))
* add transactional provider model favourites ([53aeaa3](https://github.com/matt-riley/waffle/commit/53aeaa36db47c9c921a8b50aa9ede256a8de6bb7))
* browse and favourite provider models ([8ba39a8](https://github.com/matt-riley/waffle/commit/8ba39a8247a5caf54b6e609c91707741f88f513b))
* cache provider model catalogues ([f53613b](https://github.com/matt-riley/waffle/commit/f53613bf570bff3c2ce4a887702c0a4d1654f89a))
* discover Anthropic model catalogues ([62a92b2](https://github.com/matt-riley/waffle/commit/62a92b2e709d77650f7b5d39b207f44ee4665ae7))
* discover OpenAI-compatible model catalogues ([222698e](https://github.com/matt-riley/waffle/commit/222698e1951161be22e42d63243ecdd876ce9d2f))
* guide provider enrollment with model discovery ([b5c7587](https://github.com/matt-riley/waffle/commit/b5c75876304c222ccc44a3e219d880b6604eb0a0))
* simplify managed multi-provider deployment ([3232e6d](https://github.com/matt-riley/waffle/commit/3232e6d27fa4ca4c333ae02df160d17f46361daf))


### Bug Fixes

* allow headless gateway startup ([37457d8](https://github.com/matt-riley/waffle/commit/37457d898aa5ccc10b6dfe90f50d01befe9fe6dd))
* allow headless identity generation ([8e8793b](https://github.com/matt-riley/waffle/commit/8e8793bf42f06351b58396491ef56e89e81ed923))
* close model catalogue review findings ([5ef9939](https://github.com/matt-riley/waffle/commit/5ef9939b2fd834e114c44c3b7a3fa5b70c74f75a))
* harden guided provider discovery ([218242e](https://github.com/matt-riley/waffle/commit/218242ec0d5de4eed3b0f85c4d210280fe726a25))
* harden model catalogue cache ([fc9b201](https://github.com/matt-riley/waffle/commit/fc9b201f44e626b4aa92e3222306b1abf6d14238))
* harden provider catalogue boundaries ([95f2a59](https://github.com/matt-riley/waffle/commit/95f2a59594e49fdd83ec24a318ca24caad76e3ed))
* harden provider model favourite transactions ([838d9d5](https://github.com/matt-riley/waffle/commit/838d9d51f64af940afbeea816d08dee3bd593c3e))
* honor empty provider registry in chat ([4e599b0](https://github.com/matt-riley/waffle/commit/4e599b007cbfefc50092c7215c8b9d6293c6398a))
* initialize sandbox queue before Docker ([85c8b89](https://github.com/matt-riley/waffle/commit/85c8b8908ef9b07f57e9a6a0df7195124f78c4fd))
* make infra handoff opt-in ([06b80eb](https://github.com/matt-riley/waffle/commit/06b80eb915d71476c0778b9ff26abcc86513c14f))
* prevent Anthropic catalogue error leaks ([2c9d270](https://github.com/matt-riley/waffle/commit/2c9d2707ce88f83280affb17848390fe1c95b3ca))
* redact credentials from catalogue errors ([bcbd8db](https://github.com/matt-riley/waffle/commit/bcbd8db0974d32fd2dfd73432312f3cad1e9e2be))
* reject providerless chat explicitly ([edee19d](https://github.com/matt-riley/waffle/commit/edee19d57c595db9d70d1a7126dd5aaaadadc7f8))
* satisfy provider catalogue lint checks ([562f0ce](https://github.com/matt-riley/waffle/commit/562f0ce77e853ad18c14215364d276f83370de19))

## [0.2.0](https://github.com/matt-riley/waffle/compare/v0.1.0...v0.2.0) (2026-07-18)


### Features

* **#79:** complete code-intelligence acceptance criteria ([bf891c1](https://github.com/matt-riley/waffle/commit/bf891c1b0cfac930558c020314e497394ea1a2d0))
* add deterministic rig motion evaluator ([ef79c63](https://github.com/matt-riley/waffle/commit/ef79c63252a54cf7b5ea96d42b8bede369c1e7aa))
* add durable run metric accounting ([d78d5a7](https://github.com/matt-riley/waffle/commit/d78d5a7e2b8afb9cd8543ab06bb7a998fc39be68))
* add native search tool ([74f11ec](https://github.com/matt-riley/waffle/commit/74f11ec4263e85676766deb67eba6753a17f71fa))
* add run observability status ([6027130](https://github.com/matt-riley/waffle/commit/6027130ed1ca2cceb7a58e56946d98facfca68ab))
* add source-locked Waffle rig layers ([4d3ce3c](https://github.com/matt-riley/waffle/commit/4d3ce3c604ed32c540b6ef511ef88cd5c77ac203))
* add standing rig v2 art variants ([dee3752](https://github.com/matt-riley/waffle/commit/dee3752056a56eb9b29a95326db845deaef0d356))
* add Waffle expression and paw-wave rig ([43e6deb](https://github.com/matt-riley/waffle/commit/43e6debe510fd0f5def35bd59c3a197c89149d98))
* add Waffle raster documentation poses ([7bb262a](https://github.com/matt-riley/waffle/commit/7bb262aa160df38b8b7ea222859b4d5a6585f153))
* add Waffle raster master and expressions ([c2649c1](https://github.com/matt-riley/waffle/commit/c2649c1281a064b133321878286151dff5b2219d))
* add Waffle raster model sheet ([4f93eae](https://github.com/matt-riley/waffle/commit/4f93eaefe84d22aa9ab371473b3388453a4fc6e0))
* add waffle status command ([3477ecb](https://github.com/matt-riley/waffle/commit/3477ecb71cdda1deb1a08a5b6431f98212d4d0f8))
* animate Waffle walk in place ([f03ba84](https://github.com/matt-riley/waffle/commit/f03ba846707c82c1f909436c5c87bc2e79df6109))
* close remaining AC gaps for [#79](https://github.com/matt-riley/waffle/issues/79) [#77](https://github.com/matt-riley/waffle/issues/77) [#71](https://github.com/matt-riley/waffle/issues/71) [#66](https://github.com/matt-riley/waffle/issues/66) [#63](https://github.com/matt-riley/waffle/issues/63) [#29](https://github.com/matt-riley/waffle/issues/29) [#70](https://github.com/matt-riley/waffle/issues/70) ([bc36289](https://github.com/matt-riley/waffle/commit/bc36289d223954574b58b5429de92cb288d4d5eb))
* close remaining acceptance criteria across backlog issues ([4b3c270](https://github.com/matt-riley/waffle/commit/4b3c270432c9f19ba243ce563c88072d0584620a))
* complete persisted agent-group routing ([39e11db](https://github.com/matt-riley/waffle/commit/39e11db3075eecc1509f82eb60ce09efe05f2ab2))
* complete phase 3 lifecycle and operations ([e81923a](https://github.com/matt-riley/waffle/commit/e81923adacc038af9428545c1d8f1e3262338842))
* complete phase 4 intake and extensibility ([cc03ebb](https://github.com/matt-riley/waffle/commit/cc03ebb8b5fd4e795ae0720294f828849688c0ea))
* complete reliability and trust hardening ([9d687c6](https://github.com/matt-riley/waffle/commit/9d687c6c21e548ed00141120275819e3e1ed9ff5))
* complete remaining [#72](https://github.com/matt-riley/waffle/issues/72) backlog issues ([8b6049c](https://github.com/matt-riley/waffle/commit/8b6049c83a01685f2798a2d14b6bea655db6590e))
* complete Waffle layered raster rig ([2b5d327](https://github.com/matt-riley/waffle/commit/2b5d327d33a7fd7b1cc71811064ba410255e749b))
* **gateway:** route conversations by agent group ([dfa0bca](https://github.com/matt-riley/waffle/commit/dfa0bca9f8c8716a30922eba308b97b179d736d7))
* initial commit ([5853bf2](https://github.com/matt-riley/waffle/commit/5853bf2952e47b8fc15e8b3eb1d6e333e5963193))
* instrument gateway and cron runs ([5833bbe](https://github.com/matt-riley/waffle/commit/5833bbef3f3f2b7614584d6a432d348493876dee))
* limit sandbox container resources ([63bd92a](https://github.com/matt-riley/waffle/commit/63bd92a508feea8665edd6b947bf96145b17c919))
* partition Waffle standing rig v2 ([ce02da5](https://github.com/matt-riley/waffle/commit/ce02da5b46f1a565f1192e2a3f877a5740664487))
* Phase 0 backlog unblockers ([#24](https://github.com/matt-riley/waffle/issues/24), [#25](https://github.com/matt-riley/waffle/issues/25), [#32](https://github.com/matt-riley/waffle/issues/32), [#48](https://github.com/matt-riley/waffle/issues/48)) ([#73](https://github.com/matt-riley/waffle/issues/73)) ([811c319](https://github.com/matt-riley/waffle/commit/811c319e94421de60ca40b0aba489c070d0a3633))
* Phase 1 policy spine — linux runner guard and agent-group trust tiering ([#75](https://github.com/matt-riley/waffle/issues/75)) ([88507e6](https://github.com/matt-riley/waffle/commit/88507e60b7bc251d057a61be45d23066483dcedd))
* probe provider in doctor ([a731aea](https://github.com/matt-riley/waffle/commit/a731aea038fb0f2f151b2b08ce2061563c3dbd7c))
* render rig motion review frames ([6b481a9](https://github.com/matt-riley/waffle/commit/6b481a97b1066620292146ee787e628506675311))
* serve local observability status ([b6ffcf4](https://github.com/matt-riley/waffle/commit/b6ffcf45461cc9d2deb113d49771d076d9eb1277))
* **serve:** build configured gateway agent groups ([45ad769](https://github.com/matt-riley/waffle/commit/45ad769be15359ecf7258aed5543d088ab92c42a))


### Bug Fixes

* **#29,#69,#79,#66:** stress integrity, spill docs, codeintel stale tests ([0d54399](https://github.com/matt-riley/waffle/commit/0d5439922737c4be18c23080694a5f27dc071f1d))
* **#43,#44:** enforce usage and memory notifications ([b1f2561](https://github.com/matt-riley/waffle/commit/b1f2561f04aa5aa3a2b2d56125b84ac070f33e7a))
* **#43,#44:** wire runtime alerts and provenance ([acc9075](https://github.com/matt-riley/waffle/commit/acc9075cac35d209e1c827b45eaaf5c3e2d4a44e))
* **#48:** enforce single serve owner ([4cb70de](https://github.com/matt-riley/waffle/commit/4cb70debb42282d4fd9c7500dd17cf96e10d7f23))
* **#48:** hold atomic serve advisory lock ([b35c1e0](https://github.com/matt-riley/waffle/commit/b35c1e011651a717fa0e74745fc5f424f42f4eba))
* **#53,#54,#60,#65,#78:** policy cache, hook logs, FTS, distill gate, handoffs ([da8a4a3](https://github.com/matt-riley/waffle/commit/da8a4a35a84e592b50dbcd19d8861fb9f74e64ab))
* **#59,#61,#70:** turn/cron reflection, utility model, workset maintenance ([3e8ae7d](https://github.com/matt-riley/waffle/commit/3e8ae7d245b35f9e951cdf3df34e196b466f200c))
* **#62:** enforce self-development review gates ([056b383](https://github.com/matt-riley/waffle/commit/056b383e43e5eaec0c5189e00476deeb444296d6))
* **#68:** make parallel subagent broadcast test race-safe ([a774e2c](https://github.com/matt-riley/waffle/commit/a774e2ca1655ec5754d0d4121ffe079df930602f))
* **#71,#78,#68:** profile-aware spawn, handoff repair, and broadcast tests ([2f18a5b](https://github.com/matt-riley/waffle/commit/2f18a5b13b596bd72af2c3adf9bb7e9b35400855))
* **#71:** preserve authoritative denial profile ([0e05d5b](https://github.com/matt-riley/waffle/commit/0e05d5b7bcc15692caaef49755096e347d9dbb98))
* **#82:** require exact Telegram mention ([463b79f](https://github.com/matt-riley/waffle/commit/463b79f5010edb749a04b73c6b3f82b063c80357))
* account for Anthropic cache tokens ([4117613](https://github.com/matt-riley/waffle/commit/4117613442380611b32e4c31e6b65c0bc3daae47))
* **agent:** carry context summary as system text, not a leading assistant message ([#30](https://github.com/matt-riley/waffle/issues/30)) ([3b325c2](https://github.com/matt-riley/waffle/commit/3b325c241daa1c2146fae7f54c917559843e2adf))
* bound status command request time ([28b437e](https://github.com/matt-riley/waffle/commit/28b437e5e2b7fbd44e3a8ee9115be9b776101e32))
* close acceptance audit review gaps ([838c316](https://github.com/matt-riley/waffle/commit/838c316f4a3d0895fc4168350ac0b8a106dbb1ba))
* close scheduler and budget review gaps ([7076b2f](https://github.com/matt-riley/waffle/commit/7076b2f2af5c648d274662822a2a4d8f69791e9e))
* close SVG external asset bypasses ([f5791f9](https://github.com/matt-riley/waffle/commit/f5791f9ef6196c6e79fafaf402cbcf6f4a19f643))
* **cron:** run manual `cron run` on the restricted cron tier (follow-up to [#75](https://github.com/matt-riley/waffle/issues/75)) ([#76](https://github.com/matt-riley/waffle/issues/76)) ([86184f6](https://github.com/matt-riley/waffle/commit/86184f65e4e30d29bc33a3943598bfb3837ff906))
* enforce rig control binding shapes ([f88a788](https://github.com/matt-riley/waffle/commit/f88a788ecc56c18ad249fb9f42bf4459cd21254c))
* **gateway:** use detached drain context for in-flight handlers on shutdown ([#31](https://github.com/matt-riley/waffle/issues/31)) ([f818c99](https://github.com/matt-riley/waffle/commit/f818c99d3e38f664b4eea8c4f7daac29633de7e9))
* handle deferred cleanup results ([13406ad](https://github.com/matt-riley/waffle/commit/13406ad451daccec5d13dda527dd3c07052e4a54))
* handle deferred close errors instead of nolint ([6d4754d](https://github.com/matt-riley/waffle/commit/6d4754d285e7e242b9b35a255d07349a6fe9b1aa))
* harden broker budgets and shutdown ([20392de](https://github.com/matt-riley/waffle/commit/20392def023d632f370457cb2b2328c5457186d2))
* harden rig motion review rendering ([465dc36](https://github.com/matt-riley/waffle/commit/465dc36c41e8b81e2024d8ecee38ec3ddfacbd97))
* harden standing rig v2 art variants ([8aa5dd5](https://github.com/matt-riley/waffle/commit/8aa5dd557581b78223269604b6e27e6b5a57f398))
* harden standing rig v2 builder ([719d52b](https://github.com/matt-riley/waffle/commit/719d52bea939b0fded17e2038e2b16c8a486c4d9))
* harden standing rig v2 controls ([2a6877a](https://github.com/matt-riley/waffle/commit/2a6877a0848e5b31caba74e84109313dd3c1be72))
* harden standing rig v2 validation ([4555c96](https://github.com/matt-riley/waffle/commit/4555c9643651e63cafdca29429f16a9a5d1310f3))
* harden Waffle raster asset pipeline ([2371689](https://github.com/matt-riley/waffle/commit/2371689d213c53ddeee1ddb26419938ef2d9ba6f))
* ignore transparent RGB in rig comparison ([a40fae4](https://github.com/matt-riley/waffle/commit/a40fae4ab733e2eaa10d15693b8df48191fa27b8))
* neutralize keyed art edge spill ([caaed74](https://github.com/matt-riley/waffle/commit/caaed7407f3347f87fd5626847e533d9b6033a40))
* preserve neutral rig motion renders ([0437e13](https://github.com/matt-riley/waffle/commit/0437e13a7c817a24c6ef87f5e27252e06770a5f8))
* reject external SVG stylesheets ([ff63b55](https://github.com/matt-riley/waffle/commit/ff63b55f44b6903cab246fe6246ceeda71cc6b3d))
* remove all nolint suppressions and handle errors properly ([fbcbe0b](https://github.com/matt-riley/waffle/commit/fbcbe0b4b06beb1ec61312791693e11dfc4ff2fc))
* require own rig binding fields ([8a146e9](https://github.com/matt-riley/waffle/commit/8a146e9d50bde9ec04d28712bc64d3cdd1922bd7))
* resolve golangci-lint failures on main ([241eaa0](https://github.com/matt-riley/waffle/commit/241eaa0cb93aba01281599f9200cd4f561a4ab92))
* **review:** address issue 4 (ID generators return errors instead of panic) ([#4](https://github.com/matt-riley/waffle/issues/4)) ([3e6187e](https://github.com/matt-riley/waffle/commit/3e6187e950042bc617b8b05fa0930b0eba9807f4))
* **review:** address issue 6 (bound OpenAI stream accumulation) ([#6](https://github.com/matt-riley/waffle/issues/6)) ([e742641](https://github.com/matt-riley/waffle/commit/e742641a5b4544546040d74eaa817090ddb37cbf))
* **review:** address issues 1,2,10 (workspace Open race, Close state, best-effort errors) ([#3](https://github.com/matt-riley/waffle/issues/3)) ([1ebfc3d](https://github.com/matt-riley/waffle/commit/1ebfc3d9f0d9d1af343c8dd3baf3f8658abd9fce))
* **review:** address issues 3,14 (agent summarization + bounded concurrency) ([#2](https://github.com/matt-riley/waffle/issues/2)) ([b621e99](https://github.com/matt-riley/waffle/commit/b621e99aab768fa0d2c5bffbfa69715c36d59034))
* **review:** address issues 5,13 (sandbox timeouts/dead-runner + early truncation) ([#5](https://github.com/matt-riley/waffle/issues/5)) ([840b7c8](https://github.com/matt-riley/waffle/commit/840b7c870456638ac4e071f59ed0bbd92f12795f))
* **review:** address issues 7,8,9,11,12 (cross-cutting robustness) ([#7](https://github.com/matt-riley/waffle/issues/7)) ([f84a39e](https://github.com/matt-riley/waffle/commit/f84a39e6cf396e194a581936083d3e19197e226e))
* **schedule:** await scheduler on shutdown and reconcile jobs while serving ([#10](https://github.com/matt-riley/waffle/issues/10), [#17](https://github.com/matt-riley/waffle/issues/17)) ([#57](https://github.com/matt-riley/waffle/issues/57)) ([cde1b22](https://github.com/matt-riley/waffle/commit/cde1b22f91126473cd96577116b233f2c4ce6677))
* **secret:** refuse identity init when the keyring cannot be read ([#11](https://github.com/matt-riley/waffle/issues/11)) ([#56](https://github.com/matt-riley/waffle/issues/56)) ([16bf767](https://github.com/matt-riley/waffle/commit/16bf7671b54d1e5eb52f4462fbbacaf98b072534))
* support GNU tar artifact archives ([e7b0af0](https://github.com/matt-riley/waffle/commit/e7b0af01af442ff0b16b6be734b4c5bd0b43d56e))
* use portable tar ownership flags ([2c2d87d](https://github.com/matt-riley/waffle/commit/2c2d87dff68ef9254f64219601ecec01990e3481))
* validate configured child capabilities ([cbea248](https://github.com/matt-riley/waffle/commit/cbea2485e92e9649bea3a4ac6c879f9777d3eb64))
* validate cron delivery targets ([9ca3e0d](https://github.com/matt-riley/waffle/commit/9ca3e0d585d0676aa958b4316e2f78756756907d))
* validate workspace close arguments ([8618369](https://github.com/matt-riley/waffle/commit/861836914a11bd5f65e335f628b0847b805bc013))


### Performance Improvements

* reduce SQLite lifecycle blocking ([#84](https://github.com/matt-riley/waffle/issues/84)) ([c5b9870](https://github.com/matt-riley/waffle/commit/c5b987049791a282f65cac3959d7940cc3ccf6b0))
