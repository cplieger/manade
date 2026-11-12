# Connecting to GitHub

This page shows how to give manade a GitHub App or a personal access token as its credential. It is for whoever sets up the pool.

manade registers its runners on GitHub, reads which jobs wait, and checks that every repository it serves is private. It needs no webhook and no callback address, so the App has none.

## Creating the app

Create the App on the account that owns the repositories, which is your personal account or the organization.

1. On GitHub, open **Settings**, then **Developer settings**, then **GitHub Apps**, and select **New GitHub App**. For an organization, open the organization's **Settings** first.
2. Enter a name, such as `manade on build-host`, and any homepage URL, such as your repository's address.
3. Under **Webhook**, clear **Active**.
4. Under **Repository permissions**, set **Administration** to **Read and write** and **Actions** to **Read-only**. **Metadata** is read-only already.
5. For an organization scope, also set **Self-hosted runners** under **Organization permissions** to **Read and write**.
6. Under **Where can this GitHub App be installed?**, select **Only on this account**, then select **Create GitHub App**.
7. On the App's page, copy the **Client ID**. It goes in `.env` as `GITHUB_APP_CLIENT_ID`.
8. Under **Private keys**, select **Generate a private key**. Your browser saves a `.pem` file. Save it as `config/github-app.pem`.
9. In the left menu, select **Install App**, then **Install** next to the account.
10. Select **Only select repositories**, pick the repositories the pool serves, and select **Install**.

GitHub requires Administration write to manage a repository's runners. The same permission covers the repository's settings, collaborators and deletion. So install the App on the served repositories only, keep the key readable by the pool alone, and use the App for nothing else. The pool's own reads ask GitHub for a token narrowed to read access. The Actions permission lets it read the queued jobs, which the pool's one queue is built from.

When you add a repository to `scopes` later, also add it to the App's installation under **Repository access**, or the start fails with an error that names it.

## What manade checks at start

- Each repository must be private. A public one stops the start, and one that turns public later stops being served within about two minutes.
- An organization scope uses the runner group `runner_group` names, `Default` unless set. The group must not allow public repositories. GitHub leaves that setting off for new groups.
- manade looks up the App's installation for each scope. Set `installation_id` on a scope to skip the lookup. The start still checks that the installation owns the scope.

## Using a personal access token

A token works where you cannot create an App, and an enterprise scope accepts nothing else. Set it as `GITHUB_TOKEN`, or point `GITHUB_TOKEN_FILE` at a file that holds it, and leave both App settings unset.

- For repositories, create a fine-grained token under **Settings**, then **Developer settings**, then **Personal access tokens**. Select **Only select repositories**, pick the served ones, and set **Administration** to **Read and write** and **Actions** to **Read-only**.
- For an organization, create a fine-grained token whose resource owner is the organization, with **Self-hosted runners** set to **Read and write**.
- For an enterprise, create a classic token with the `manage_runners:enterprise` scope. GitHub refuses App authentication for enterprise runners.

Avoid a classic token for repositories. It needs the `repo` scope, which is full control of every private repository you can reach. A token also shares its owner's limit of 5,000 API requests an hour with every other tool that uses it, where each App installation gets its own.
