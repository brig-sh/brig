# AI policy

AI-assisted development is welcome in Brig.

We encourage contributors to use AI tools, coding agents, automated
reasoning, and other software-engineering systems where they are useful. We
are also interested in experiments with frontier models, agentic workflows,
automated debugging, testing, code review, and other new ways of building
reliable software.

AI is a development tool. Using it does not make a contribution more or less
valuable by itself. What matters is the quality of the result.

## Disclosure is not required

You do not need to disclose that AI was used to create a contribution.

You are welcome to describe the models, agents, prompts, workflows, or
experiments that helped produce your work when that information is
interesting or useful. Ordinary AI assistance does not need a label,
footer, or declaration.

When the generation process itself matters for reproducing or evaluating the
contribution, document that process. Examples include an AI benchmark, a
generated test corpus, or an experiment with autonomous agents.

## Responsibility does not transfer to the model

A contribution is judged as a contribution to Brig, not as AI output.

By submitting code, documentation, tests, benchmarks, or other material, you
take responsibility for it as if you had written every line yourself.

Before submitting a change:

* Understand what the change is intended to do.
* Review the resulting diff.
* Run the applicable tests and repository gates.
* Verify claims about correctness, performance, compatibility, and security.
* Remove speculative, irrelevant, duplicated, or unnecessary changes.

Do not claim that tests passed, a bug was reproduced, a benchmark improved,
or a behavior was verified unless that actually happened.

"An AI generated it" is neither an excuse for a defect nor evidence that
something is correct.

## Respect maintainer attention

AI can make producing code cheap. Reviewing code is not cheap.

Do not use that asymmetry to transfer work to maintainers. Large quantities
of unreviewed generated code, speculative fixes, issue-farming, or
mechanical repository-wide rewrites can be closed. The same applies to a
pull request whose author has made little effort to establish correctness.

A contribution must make the project better, not merely make the diff
larger.

Small, well-understood changes are usually easier to evaluate than broad
changes made only because an agent can produce them.

## AI does not relax Brig's engineering rules

All repository rules, architectural decisions, compatibility requirements,
and correctness invariants apply equally to AI-assisted and manually written
changes.

An agent finding an easy implementation is not a reason to bypass an
architectural boundary.

Changes to durable formats, consistency semantics, protocols, public
behavior, or other architectural contracts need the same design
consideration and review as any other change. This is true whether or not
AI helped write it. Where the
repository requires a specification change, compatibility analysis, or
particular test coverage, AI-assisted work must satisfy those requirements
too.

Prefer evidence over confidence. Tests, fault injection, benchmarks,
reproductions, and clear reasoning are more useful than an agent's assertion
that a change is correct.

## Agentic contributions are welcome

Brig development can use interactive assistants as well as autonomous or
unattended agents.

Give an agent bounded tasks, sufficient repository context, and objective
verification criteria. Inspect and validate its output before it becomes
part of the project.

Autonomy increases the importance of verification. It does not reduce it.

Repository-maintained automation can create commits, pull requests, reports,
or other operational messages as part of established workflows. External
automation must not flood issues, pull requests, reviews, or discussions.

## Communicating with maintainers

AI can help you understand review feedback, investigate a problem, or
improve the clarity of a response.

Do not use it to generate high-volume or non-responsive discussion. A reply
to review feedback must show that you considered the feedback and, when
appropriate, that you inspected or tested the underlying code.

Never invent explanations, measurements, reproductions, citations, or
technical conclusions because a model produced plausible text.

Maintainer conversation is part of engineering the change, not an obstacle
to automate away.

## Prefer AI where verification is strong

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
the design before generating the implementation.

## Security and sensitive data

Do not expose secrets, credentials, private telemetry, vulnerability reports
under embargo, proprietary code, or personal data to an AI service. Do not
disclose any other information you are not authorized to share.

Contributors are responsible for understanding how the tools they use handle
data.

AI-generated code must meet the same security expectations as any other
code. Give generated dependency additions, cryptographic code, parsers,
authentication logic, unsafe input handling, and security-sensitive
configuration extra scrutiny.

## Intellectual property and provenance

The same intellectual-property and licensing rules apply regardless of how a
contribution was produced.

Do not submit code, documentation, tests, or other material copied or
reproduced from another project unless you have the right to do so. You must
meet every applicable license and attribution requirement.

AI output does not remove provenance or licensing obligations.

You are responsible for making sure you have the legal right to contribute
the material you submit.

## Research is welcome

We welcome research involving Brig and AI-assisted software engineering,
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

If an experiment produces a genuinely useful improvement to Brig, we welcome
it as a contribution.

---

Use AI as much as you like, but verify what you submit, respect maintainer
attention, and follow Brig's engineering rules. Take responsibility for the
result the same way you take responsibility for code you write by hand.
