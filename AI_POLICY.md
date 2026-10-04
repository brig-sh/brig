# AI policy

AI-assisted development is welcome in Brig.

We encourage contributors to use AI tools, coding agents, automated
reasoning, and other software-engineering systems where they are useful. We
are also interested in experiments with frontier models, agentic workflows,
automated debugging, testing, code review, and other new ways to build
reliable software.

AI is a development tool. The use of AI does not make a contribution more
valuable or less valuable. The quality of the result decides.

## Disclosure is not required

You do not need to disclose that AI helped create a contribution. Ordinary
AI assistance does not need a label, footer, or declaration.

You are welcome to describe the models, agents, prompts, workflows, or
experiments behind your work when that information is interesting or useful.

When the generation process matters for reproducing or evaluating the
contribution, document that process. Examples are an AI benchmark, a
generated test corpus, and an experiment with autonomous agents.

## Responsibility stays with you

Maintainers judge a contribution as a contribution to Brig, not as AI
output.

When you submit code, documentation, tests, benchmarks, or other material,
you take responsibility for it as if you wrote every line yourself.

Before you submit a change:

* Understand what the change is intended to do.
* Review the resulting diff.
* Run the applicable tests and repository gates.
* Verify claims about correctness, performance, compatibility, and security.
* Remove speculative, irrelevant, duplicated, or unnecessary changes.

Do not claim that tests passed, a bug was reproduced, a benchmark improved,
or a behavior was verified unless that happened.

"An AI generated it" does not excuse a defect. It is also not evidence that
something is correct.

## Respect maintainer attention

AI makes code cheap to produce. Review still costs maintainers time. Do not
use that difference to transfer work to maintainers.

Maintainers can close any of these:

* Large quantities of unreviewed generated code.
* Speculative fixes.
* Issue-farming.
* Mechanical repository-wide rewrites.
* A pull request whose author made little effort to establish correctness.

A contribution must make the project better. A larger diff alone does not.

Small, well-understood changes are usually easier to evaluate than broad
changes made only because an agent can produce them.

## Engineering rules still apply

All repository rules, architectural decisions, compatibility requirements,
and correctness invariants apply equally to AI-assisted and manually written
changes.

An easy implementation that an agent finds is not a reason to bypass an
architectural boundary.

Changes to durable formats, consistency semantics, protocols, public
behavior, or other architectural contracts need the same design
consideration and review as any other change. This applies with or without
AI. Where the repository requires a specification change, compatibility
analysis, or particular test coverage, AI-assisted work must satisfy those
requirements too.

Prefer evidence over confidence. Tests, fault injection, benchmarks,
reproductions, and clear reasoning are more useful than the assertion of an
agent that a change is correct.

## Agentic contributions are welcome

Brig development can use interactive assistants, and autonomous or
unattended agents.

Give an agent bounded tasks, sufficient repository context, and objective
verification criteria. Inspect and validate its output before it becomes
part of the project.

More autonomy makes verification more important, not less.

Repository-maintained automation can create commits, pull requests, reports,
or other operational messages as part of established workflows. External
automation must not flood issues, pull requests, reviews, or discussions.

## Communicating with maintainers

AI can help you understand review feedback, investigate a problem, or
make a response clearer.

Do not use AI to generate high-volume or non-responsive discussion. A reply
to review feedback must show that you considered the feedback. When
appropriate, it must also show that you inspected or tested the underlying
code.

Never invent explanations, measurements, reproductions, citations, or
technical conclusions because a model produced plausible text.

A conversation with a maintainer is part of the engineering work on the
change. Do not automate it away.

## Prefer verifiable work

AI is most useful when its output can be checked objectively. Examples
include:

* Finding and fixing bugs.
* Adding regression and property tests.
* Simplifying or deleting unnecessary code.
* Improving error handling.
* Fuzzing and fault-injection work.
* Improving build and CI tooling.
* Identifying performance regressions.
* Improving documentation.
* Analyzing concurrency or failure paths.
* Detecting inconsistencies between implementation and specification.
* Security research and defensive analysis.

Large features are welcome too, but AI does not replace design. The larger
the semantic or architectural change, the more important it is to establish
the design before you generate the implementation.

## Security and sensitive data

Do not expose any of these to an AI service:

* Secrets and credentials.
* Private telemetry.
* Vulnerability reports under embargo.
* Proprietary code.
* Personal data.
* Any other information that you are not authorized to share.

You are responsible for understanding how the tools you use handle data.

AI-generated code must meet the same security expectations as any other
code. Give extra scrutiny to generated dependency additions, cryptographic
code, parsers, authentication logic, unsafe input handling, and
security-sensitive configuration.

## Intellectual property and provenance

The same intellectual-property and licensing rules apply to every
contribution, however it was produced. AI output does not remove provenance
or licensing obligations.

Do not submit code, documentation, tests, or other material copied or
reproduced from another project unless you have the right to do so. You must
meet every applicable license and attribution requirement.

You are responsible for making sure that you have the legal right to
contribute the material you submit.

## Research is welcome

We welcome research that involves Brig and AI-assisted software engineering,
including:

* Model comparisons and identical-task evaluations.
* Automated bug repair.
* Test generation.
* Agentic development loops.
* Reproducibility studies.
* Code-review experiments.
* Security analysis.
* Failure-injection experiments.
* New approaches to autonomous software engineering.

Research must preserve the same standards of safety, licensing, and
repository integrity as ordinary development.

If an experiment produces a useful improvement to Brig, we welcome it as a
contribution.
