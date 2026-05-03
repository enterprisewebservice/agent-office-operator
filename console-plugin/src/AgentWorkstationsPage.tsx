import * as React from 'react';
import {
  K8sResourceCommon,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
// Console wraps plugin pages in its own <Page> — only PageSection here.
import { PageSection } from '@patternfly/react-core/dist/dynamic/components/Page';
import { Title } from '@patternfly/react-core/dist/dynamic/components/Title';
import { Card, CardBody, CardHeader, CardTitle } from '@patternfly/react-core/dist/dynamic/components/Card';
import { Label } from '@patternfly/react-core/dist/dynamic/components/Label';
import { Spinner } from '@patternfly/react-core/dist/dynamic/components/Spinner';
import { EmptyState, EmptyStateBody } from '@patternfly/react-core/dist/dynamic/components/EmptyState';
import { Button } from '@patternfly/react-core/dist/dynamic/components/Button';
import { Bullseye } from '@patternfly/react-core/dist/dynamic/layouts/Bullseye';
import { Flex, FlexItem } from '@patternfly/react-core/dist/dynamic/layouts/Flex';
import { Gallery } from '@patternfly/react-core/dist/dynamic/layouts/Gallery';
import ExternalLinkAltIcon from '@patternfly/react-icons/dist/dynamic/icons/external-link-alt-icon';

type AgentWorkstation = K8sResourceCommon & {
  spec?: {
    displayName?: string;
    description?: string;
    emoji?: string;
    image?: string;
    apiKeySecretRef?: string;
    model?: { provider?: string; modelName?: string };
    tools?: { allow?: string[] };
    memory?: { modules?: Array<{ name: string }> };
  };
  status?: {
    phase?: string;
    gatewayEndpoint?: string;
    message?: string;
  };
};

const phaseColor = (phase?: string) => {
  switch (phase) {
    case 'Running':
      return 'green';
    case 'Creating':
      return 'orange';
    case 'Pending':
      return 'gold';
    case 'Stopped':
      return 'grey';
    case 'Error':
      return 'red';
    default:
      return 'grey';
  }
};

const AgentWorkstationsPage: React.FC = () => {
  const [agents, loaded, loadError] = useK8sWatchResource<AgentWorkstation[]>({
    groupVersionKind: { group: 'agentoffice.ai', version: 'v1alpha1', kind: 'AgentWorkstation' },
    namespace: 'agent-office',
    isList: true,
  });

  const devSpacesUrl = (name: string) =>
    `https://devspaces.apps.salamander.aimlworkbench.com/#https://github.com/enterprisewebservice/${name}-agent`;

  return (
    <>
      <PageSection variant="light">
        <Title headingLevel="h1">Agent Workstations</Title>
        <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
          Governed coding-agent instances reconciled by the Agent Office Operator. Each card shows
          live phase + endpoint, the model provider, tool allowlist, and the shared memory modules
          the agent pulls in.
        </p>
      </PageSection>
      <PageSection>
        {!loaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {loadError && <p>Failed to load: {String(loadError)}</p>}
        {loaded && agents?.length === 0 && (
          <EmptyState>
            <EmptyStateBody>
              No AgentWorkstations in the agent-office namespace yet. Create one from the agent-office UI.
            </EmptyStateBody>
          </EmptyState>
        )}
        {loaded && agents && agents.length > 0 && (
          <Gallery hasGutter>
            {agents.map((a) => {
              const memoryRefs = a.spec?.memory?.modules ?? [];
              const tools = a.spec?.tools?.allow ?? [];
              return (
                <Card key={a.metadata?.uid} isCompact>
                  <CardHeader>
                    <CardTitle>
                      <Flex>
                        <FlexItem>
                          <span style={{ fontSize: 24, marginRight: 8 }}>{a.spec?.emoji}</span>
                          <strong>{a.spec?.displayName || a.metadata?.name}</strong>
                        </FlexItem>
                        <FlexItem align={{ default: 'alignRight' }}>
                          <Label color={phaseColor(a.status?.phase)}>
                            {a.status?.phase ?? 'Unknown'}
                          </Label>
                        </FlexItem>
                      </Flex>
                    </CardTitle>
                  </CardHeader>
                  <CardBody>
                    {a.spec?.description && (
                      <p style={{ marginBottom: 8, color: 'var(--pf-v5-global--Color--200)' }}>
                        {a.spec.description}
                      </p>
                    )}
                    <p style={{ marginBottom: 4, fontSize: 13 }}>
                      <strong>provider:</strong> {a.spec?.model?.provider} &nbsp;
                      <strong>model:</strong> {a.spec?.model?.modelName ?? 'auto'}
                    </p>
                    {tools.length > 0 && (
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>tools:</strong> {tools.slice(0, 3).join(', ')}
                        {tools.length > 3 ? ` +${tools.length - 3}` : ''}
                      </p>
                    )}
                    {memoryRefs.length > 0 && (
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>memory:</strong>{' '}
                        {memoryRefs.map((m) => (
                          <Label key={m.name} color="purple" style={{ marginRight: 4 }}>
                            {m.name}
                          </Label>
                        ))}
                      </p>
                    )}
                    {a.status?.gatewayEndpoint && (
                      <div style={{ marginTop: 12 }}>
                        <Button
                          component="a"
                          href={a.status.gatewayEndpoint}
                          target="_blank"
                          variant="primary"
                          icon={<ExternalLinkAltIcon />}
                        >
                          Open agent gateway
                        </Button>
                        <Button
                          component="a"
                          href={devSpacesUrl(a.metadata?.name ?? '')}
                          target="_blank"
                          variant="secondary"
                          icon={<ExternalLinkAltIcon />}
                          style={{ marginLeft: 8 }}
                        >
                          Edit in Dev Spaces
                        </Button>
                      </div>
                    )}
                  </CardBody>
                </Card>
              );
            })}
          </Gallery>
        )}
      </PageSection>
    </>
  );
};

export default AgentWorkstationsPage;
