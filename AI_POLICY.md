# brig AI Policy

AI-assisted development is welcome in brig.

We encourage contributors to use AI tools, coding agents, automated reasoning, and other software-engineering systems where they are useful. We are also interested in experiments with frontier models, agentic workflows, automated debugging, testing, code review, and other new ways of building reliable software.

AI is a development tool. Using it does not make a contribution more or less valuable by itself. What matters is the quality of the result.

## Disclosure is not required

You do not need to disclose that AI was used to create a contribution.

You are welcome to describe the models, agents, prompts, workflows, or experiments that helped produce your work when that information is interesting or useful. Ordinary AI assistance does not require a label, footer, or declaration.

When the generation process itself matters for reproducing or evaluating the contribution (for example, an AI benchmark, a generated test corpus, or an experiment with autonomous agents), documenting that process is encouraged.

## Responsibility does not transfer to the model

A contribution is judged as a contribution to brig, not as AI output.

By submitting code, documentation, tests, benchmarks, or other material, you take responsibility for it in the same way you would if you had written every line manually.

Before submitting a change:

* understand what the change is intended to do;
* review the resulting diff;
* run the applicable tests and repository gates;
* verify claims about correctness, performance, compatibility, and security;
* remove speculative, irrelevant, duplicated, or unnecessary changes.

Do not claim that tests passed, a bug was reproduced, a benchmark improved, or a behavior was verified unless that actually happened.

"An AI generated it" is neither an excuse for a defect nor evidence that something is correct.

## Respect maintainer attention

AI can make producing code cheap. Reviewing code is not cheap.

Do not use that asymmetry to transfer work to maintainers. Large quantities of unreviewed generated code, speculative fixes, issue-farming, mechanical repository-wide rewrites, or pull requests whose author has made little effort to establish correctness may be closed.

A contribution should make the project better, not merely make the diff larger.

Small, well-understood changes are usually easier to evaluate than broad changes made just because an agent was capable of producing them.

## AI does not relax brig's engineering rules

All repository rules, architectural decisions, compatibility requirements, and correctness invariants apply equally to AI-assisted and manually written changes.

An agent finding an easy implementation is not a reason to bypass an architectural boundary.

Changes to durable formats, consistency semantics, protocols, public behavior, or other architectural contracts require the same design consideration and review they would require without AI. Where the repository requires an ADR, specification change, compatibility analysis, or particular test coverage, AI-assisted work must satisfy those requirements too.

Prefer evidence over confidence: tests, fault injection, benchmarks, reproductions, and clear reasoning are more useful than an agent's assertion that a change is correct.

## Agentic contributions are welcome

brig may be developed using interactive assistants as well as autonomous or unattended agents.

Agents should be given bounded tasks, sufficient repository context, and objective verification criteria. Their output should be inspected and validated before it becomes part of the project.

Autonomy increases the importance of verification; it does not reduce it.

Repository-maintained automation may create commits, pull requests, reports, or other operational messages as part of established workflows. External automation must not be used to flood issues, pull requests, reviews, or discussions.

## Communicating with maintainers

AI can help you understand review feedback, investigate a problem, or improve the clarity of a response.

Do not use it to generate high-volume or non-responsive discussion. A reply to review feedback should show that you considered the feedback and, when appropriate, that you inspected or tested the underlying code.

Never invent explanations, measurements, reproductions, citations, or technical conclusions simply because a model produced plausible text.

Maintainer conversation is part of engineering the change, not an obstacle to automate away.

## Prefer AI where verification is strong

AI is most useful when its output can be checked objectively.

Examples include:

* finding and fixing bugs;
* adding regression and property tests;
* simplifying or deleting unnecessary code;
* improving error handling;
* fuzzing and fault-injection work;
* improving build and CI tooling;
* identifying performance regressions;
* improving documentation;
* analyzing concurrency or failure paths;
* detecting inconsistencies between implementation and specification;
* security research and defensive analysis.

Large features are welcome too, but AI does not replace design. The larger the semantic or architectural change, the more important it is to establish the design before generating the implementation.

## Security and sensitive data

Do not expose secrets, credentials, private telemetry, vulnerability reports under embargo, proprietary code, personal data, or other information you are not authorized to disclose to an AI service.

Contributors are responsible for understanding how the tools they use handle data.

AI-generated code must meet the same security expectations as any other code. Give generated dependency additions, cryptographic code, parsers, authentication logic, unsafe input handling, and security-sensitive configuration extra scrutiny.

## Intellectual property and provenance

The same intellectual-property and licensing rules apply regardless of how a contribution was produced.

Do not submit code, documentation, tests, or other material copied or reproduced from another project unless you have the right to do so and you meet every applicable license and attribution requirement.

AI output does not remove provenance or licensing obligations.

You are responsible for making sure you have the legal right to contribute the material you submit.

## Research is welcome

We welcome research involving brig and AI-assisted software engineering, including:

* model comparisons and identical-task evaluations;
* automated bug repair;
* test generation;
* agentic development loops;
* reproducibility studies;
* code-review experiments;
* security analysis;
* failure-injection experiments;
* new approaches to autonomous software engineering.

Research should preserve the same standards of safety, licensing, and repository integrity as ordinary development.

If an experiment produces a genuinely useful improvement to brig, we welcome it as a contribution.

---

The short version:

**Use AI freely. Do not submit slop. Verify what you submit. Respect maintainer attention. Follow the architecture. Take responsibility for the result.**
