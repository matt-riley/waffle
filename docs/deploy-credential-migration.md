# Deployment credential migration

This change depends on the coordinated infrastructure migration. Before merging:

1. Install a dedicated dispatch GitHub App on `matt-riley/infra`, granting only repository contents write (required by the dispatch API). It must not have Actions or Secrets administration permission.
2. Set this repository's `INFRA_DISPATCH_PRIVATE_KEY` secret first, then set `INFRA_DISPATCH_APP_ID` last to enable requests. The infra administrator App key must never be distributed to source repositories.
3. Configure infra's separate artifact-reader App to read this source repository's contents and Actions, then complete the infra migration review before merging this caller.
4. Remove the legacy source `APP_ID` variable and `PRIVATE_KEY` secret only after confirming no remaining consumer uses them. Verify a successful production-branch CI dispatch and the receiver's exact revision validation.

The reusable workflow is pinned to public `matt-riley-ci` commit `2aedbf6107ff792f9dd41b9c9074dc4801b5888c`. This PR neither creates Apps nor changes live secrets or deployments.

To disable automatic handoff, remove `INFRA_DISPATCH_APP_ID` first, then remove its dispatch key. A missing ID or key skips only the optional request, preserving successful artifact CI for manual infra reconciliation. A present but invalid key still fails authentication and must be corrected. Removing legacy `PRIVATE_KEY` in step 4 does not remove `INFRA_DISPATCH_PRIVATE_KEY`.
