import * as React from 'react';
import {
  K8sResourceCommon,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import {
  Page,
  PageSection,
  Title,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  Gallery,
  Label,
  LabelGroup,
  Spinner,
  EmptyState,
  EmptyStateBody,
  Bullseye,
  Flex,
  FlexItem,
  Button,
} from '@patternfly/react-core';
import { ExternalLinkAltIcon } from '@patternfly/react-icons';

type MemoryModule = K8sResourceCommon & {
  spec?: {
    kind?: string;
    filename?: string;
    version?: string;
    description?: string;
    source?: {
      configMapRef?: { name?: string; key?: string };
      inline?: string;
    };
  };
  status?: {
    contentSha256?: string;
    referencedBy?: string[];
    conditions?: Array<{ type: string; status: string; message?: string }>;
  };
};

const MemoryModulesPage: React.FC = () => {
  const [modules, loaded, loadError] = useK8sWatchResource<MemoryModule[]>({
    groupVersionKind: { group: 'agentoffice.ai', version: 'v1alpha1', kind: 'MemoryModule' },
    namespace: 'agent-office',
    isList: true,
  });

  // Group modules by content hash so identical content shows up as a "shared with N agents/modules" badge.
  const sharingByHash = React.useMemo(() => {
    const map = new Map<string, string[]>();
    (modules ?? []).forEach((m) => {
      const sha = m.status?.contentSha256;
      if (!sha) return;
      const arr = map.get(sha) ?? [];
      arr.push(m.metadata?.name ?? '');
      map.set(sha, arr);
    });
    return map;
  }, [modules]);

  const devSpacesUrl = (modulePath: string) =>
    `https://devspaces.apps.salamander.aimlworkbench.com/#https://github.com/enterprisewebservice/agent-office-memory-modules?path=${encodeURIComponent(
      modulePath,
    )}`;

  return (
    <Page>
      <PageSection variant="light">
        <Title headingLevel="h1">Memory Modules</Title>
        <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
          Shared <code>.md</code> content (AGENTS.md / USER.md / SKILL_*.md) referenced by one or
          more AgentWorkstations. The operator computes a content hash so the UI can show which
          agents share which memory.
        </p>
      </PageSection>
      <PageSection>
        {!loaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {loadError && <p>Failed to load: {String(loadError)}</p>}
        {loaded && modules?.length === 0 && (
          <EmptyState>
            <EmptyStateBody>
              No MemoryModules in the agent-office namespace yet. Add one in the{' '}
              <a href="https://github.com/enterprisewebservice/agent-office-memory-modules">
                library repo
              </a>
              .
            </EmptyStateBody>
          </EmptyState>
        )}
        {loaded && modules && modules.length > 0 && (
          <Gallery hasGutter>
            {modules.map((m) => {
              const sha = m.status?.contentSha256 ?? '';
              const sharedWith = (sharingByHash.get(sha) ?? []).filter(
                (n) => n !== m.metadata?.name,
              );
              const refs = m.status?.referencedBy ?? [];
              const ready = m.status?.conditions?.find((c) => c.type === 'Ready')?.status === 'True';
              const modulePath = `modules/${m.spec?.kind}/${m.metadata?.name?.replace(
                /^[^-]+-/,
                '',
              )}.md`;
              return (
                <Card key={m.metadata?.uid} isCompact>
                  <CardHeader>
                    <CardTitle>
                      <Flex>
                        <FlexItem>
                          <strong>{m.metadata?.name}</strong>
                        </FlexItem>
                        <FlexItem align={{ default: 'alignRight' }}>
                          <Label color={ready ? 'green' : 'orange'}>
                            {ready ? 'Ready' : 'Pending'}
                          </Label>
                        </FlexItem>
                      </Flex>
                    </CardTitle>
                  </CardHeader>
                  <CardBody>
                    <p style={{ marginBottom: 8 }}>
                      <strong>kind:</strong> <code>{m.spec?.kind}</code> &nbsp;
                      <strong>filename:</strong> <code>{m.spec?.filename}</code>
                    </p>
                    {m.spec?.description && (
                      <p style={{ marginBottom: 8, color: 'var(--pf-v5-global--Color--200)' }}>
                        {m.spec.description}
                      </p>
                    )}
                    <p style={{ marginBottom: 8, fontSize: 12, fontFamily: 'monospace' }}>
                      sha256: {sha ? sha.slice(0, 16) + '…' : '—'}
                    </p>
                    {sharedWith.length > 0 && (
                      <LabelGroup categoryName="Identical to">
                        {sharedWith.map((name) => (
                          <Label key={name} color="blue">
                            {name}
                          </Label>
                        ))}
                      </LabelGroup>
                    )}
                    {refs.length > 0 && (
                      <LabelGroup categoryName="Used by agents" style={{ marginTop: 8 }}>
                        {refs.map((name) => (
                          <Label key={name} color="purple">
                            {name}
                          </Label>
                        ))}
                      </LabelGroup>
                    )}
                    <div style={{ marginTop: 12 }}>
                      <Button
                        component="a"
                        href={devSpacesUrl(modulePath)}
                        target="_blank"
                        variant="secondary"
                        icon={<ExternalLinkAltIcon />}
                      >
                        Edit in Dev Spaces
                      </Button>
                    </div>
                  </CardBody>
                </Card>
              );
            })}
          </Gallery>
        )}
      </PageSection>
    </Page>
  );
};

export default MemoryModulesPage;
