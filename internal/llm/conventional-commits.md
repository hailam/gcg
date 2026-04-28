# Conventional Commits 1.0.0

Adapted from https://www.conventionalcommits.org/en/v1.0.0/. This file is
embedded into the gcg system prompt so the model has the spec at hand when
constructing a subject.

## Format

A Conventional Commits message has this structure:

```
<type>[optional scope][!]: <description>

[optional body]

[optional footer(s)]
```

gcg only generates the first line — the subject.

## Type (required)

Pick the one type that best describes the primary intent of the change:

- **feat** — a new feature (MINOR in SemVer)
- **fix** — a bug fix (PATCH in SemVer)
- **docs** — documentation only changes
- **style** — formatting, whitespace, semicolons; no code-meaning change
- **refactor** — a code change that neither fixes a bug nor adds a feature
- **perf** — a code change that improves performance
- **test** — adding or correcting tests
- **build** — changes to the build system or external dependencies
- **ci** — changes to CI configuration files and scripts
- **chore** — other changes that don't modify src or test files
- **revert** — reverts a previous commit

## Scope (optional but preferred)

A noun in parentheses identifying the section of the codebase affected.
Pick the most specific noun that covers all the changes — usually the
package, module, or feature folder name. Identify it from the staged
file paths.

The principle is the same in every ecosystem: name the area touched.
Concrete examples by stack:

- **Go** — package or top-level module:
  - `internal/auth/*` → `feat(auth):`
  - `cmd/server/*` → `fix(server):`
  - `pkg/cache/*` → `refactor(cache):`
- **JS / TS** — workspace, package, or component:
  - `packages/api/*` → `fix(api):`
  - `src/components/Button.tsx` → `feat(button):`
  - `apps/web/*` → `chore(web):`
- **PHP** (Laravel / Symfony / generic) — controller, model, or feature folder:
  - `app/Http/Controllers/AuthController.php` → `feat(auth):`
  - `app/Models/User.php` → `fix(user):`
  - `routes/api.php` → `feat(api):`
  - `src/Service/Payment/*` → `refactor(payment):`
- **Zig** — module file or directory:
  - `src/parser.zig` → `fix(parser):`
  - `lib/std/*` → `refactor(std):`
  - `build.zig` → `build(zig):`
- **Stack-agnostic** — subsystem or feature area when no single
  package fits: `feat(routing):`, `fix(auth):`, `perf(query):`,
  `chore(deps):`, `build(docker):`, `ci(github):`.

Omit the scope only when the change genuinely spans unrelated areas of
the codebase. Prefer including a scope when one is identifiable.

## Breaking changes

A breaking change is marked with `!` immediately before the colon. It
correlates with MAJOR in SemVer.

- `feat!: drop support for Go 1.21`
- `feat(api)!: rename GET /v1/users to GET /v2/users`
- `refactor(auth)!: remove deprecated SessionToken field`

## Description (required)

Short, imperative summary:

- Imperative mood: "add", not "added" or "adds"
- No trailing period
- Concrete about what changed, not how it was done
- The whole subject (type + scope + description) stays under 72 characters

## Examples

- `feat(auth): add OAuth2 login flow`
- `feat(parser): support nested array literals`
- `fix(button): correct hover-state regression`
- `fix(user): handle null email in user factory`
- `refactor(api): extract shared error response helper`
- `docs: add install instructions to README`
- `chore(deps): bump axios to v1.7.0`
- `chore(deps): require php ^8.3 in composer.json`
- `chore(deps): bump golang.org/x/text to v0.30.0`
- `chore(deps): update zig-clap to 0.9.0`
- `test(auth): cover expired-token rejection`
- `perf(query): cache prepared statements per request`
- `feat(api)!: change response shape of GET /users`
- `ci: run unit tests on every push`
- `revert: restore previous router config`
