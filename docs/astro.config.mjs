import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  site: 'https://envctl.internal',
  base: '/docs',
  integrations: [
    starlight({
      title: 'envctl',
      description: 'Isolated docker-compose environments per feature branch, for people and coding agents.',
      sidebar: [
        { label: 'Start here', items: [
          { label: 'Install', slug: 'install' },
          { label: 'Quick start', slug: 'quick-start' },
          { label: 'Concepts', slug: 'concepts' },
        ]},
        { label: 'Guides', items: [
          { label: 'How to work with envctl', slug: 'how-to-work' },
          { label: 'Agents', slug: 'agents' },
          { label: 'Agent automation', slug: 'agent-automation' },
          { label: 'Workflow fixtures (experimental)', slug: 'workflow-fixtures' },
          { label: 'CI', slug: 'ci' },
          { label: 'Troubleshooting', slug: 'troubleshooting' },
        ]},
        { label: 'Reference', items: [
          { label: 'Configuration', slug: 'configuration' },
          { label: 'Config reference', slug: 'config-reference' },
          { label: 'CLI reference', slug: 'cli-reference' },
          { label: 'Roadmap', slug: 'roadmap' },
        ]},
        { label: 'Design notes', items: [
          { label: 'Workflow runtime plan', slug: 'design/workflow-runtime' },
          { label: 'Environment identity', slug: 'design/env-identity' },
          { label: 'External dependencies', slug: 'design/external-dependencies' },
        ]},
      ],
    }),
  ],
});
