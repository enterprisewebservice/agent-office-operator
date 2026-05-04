import * as React from 'react';
import {
  K8sResourceCommon,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
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

type AgentGateway = K8sResourceCommon & {
  spec?: {
    displayName?: string;
    description?: string;
    image?: string;
    nodeHostRef?: { name?: string; namespace?: string };
    sharedTokenSecretRef?: string;
    envFromSecretRef?: string;
    autoApproveNodeHost?: boolean;
    allowedUsers?: Array<{ channel?: string; accountId?: string; id: string }>;
  };
  status?: {
    phase?: string;
    gatewayEndpoint?: string;
    nodeHostPaired?: boolean;
    agentCount?: number;
    message?: string;
  };
};

const phaseColor = (phase?: string) => {
  switch (phase) {
    case 'Ready':
      return 'green';
    case 'Provisioning':
      return 'orange';
    case 'Pending':
      return 'gold';
    case 'Error':
      return 'red';
    default:
      return 'grey';
  }
};

const AgentGatewaysPage: React.FC = () => {
  const [gateways, loaded, loadError] = useK8sWatchResource<AgentGateway[]>({
    groupVersionKind: { group: 'agentoffice.ai', version: 'v1alpha1', kind: 'AgentGateway' },
    namespace: 'agent-office',
    isList: true,
  });

  return (
    <>
      <PageSection variant="light">
        <Title headingLevel="h1">Agent Gateways</Title>
        <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
          Shared OpenClaw gateway runtimes. Each gateway pod hosts one or more
          AgentWorkstations as logical openclaw agents (per
          <code> spec.runtime.shared.gatewayRef</code>) and pairs with a single
          node-host VM that serves the browser via per-agent Chromium profiles.
        </p>
      </PageSection>
      <PageSection>
        {!loaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {loadError && <p>Failed to load: {String(loadError)}</p>}
        {loaded && gateways?.length === 0 && (
          <EmptyState>
            <EmptyStateBody>
              No AgentGateways in the agent-office namespace yet. Create one to
              host shared-runtime agents.
            </EmptyStateBody>
          </EmptyState>
        )}
        {loaded && gateways && gateways.length > 0 && (
          <Gallery hasGutter>
            {gateways.map((gw) => {
              const allowedUsers = gw.spec?.allowedUsers ?? [];
              const nodeHost = gw.spec?.nodeHostRef?.name;
              return (
                <Card
                  key={gw.metadata?.uid}
                  isCompact
                  isFullHeight
                  style={{ display: 'flex', flexDirection: 'column' }}
                >
                  <CardHeader>
                    <CardTitle>
                      <Flex>
                        <FlexItem>
                          <strong>{gw.spec?.displayName || gw.metadata?.name}</strong>
                        </FlexItem>
                        <FlexItem align={{ default: 'alignRight' }}>
                          <Label color={phaseColor(gw.status?.phase)}>
                            {gw.status?.phase ?? 'Unknown'}
                          </Label>
                        </FlexItem>
                      </Flex>
                    </CardTitle>
                  </CardHeader>
                  <CardBody style={{ display: 'flex', flexDirection: 'column', flex: 1 }}>
                    <div>
                      {gw.spec?.description && (
                        <p style={{ marginBottom: 8, color: 'var(--pf-v5-global--Color--200)' }}>
                          {gw.spec.description}
                        </p>
                      )}
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>agents:</strong> {gw.status?.agentCount ?? 0}
                      </p>
                      {nodeHost && (
                        <p style={{ marginBottom: 4, fontSize: 13 }}>
                          <strong>node-host:</strong> {nodeHost}{' '}
                          <Label
                            color={gw.status?.nodeHostPaired ? 'green' : 'grey'}
                            isCompact
                          >
                            {gw.status?.nodeHostPaired ? 'paired' : 'unpaired'}
                          </Label>
                        </p>
                      )}
                      {gw.spec?.envFromSecretRef && (
                        <p style={{ marginBottom: 4, fontSize: 13 }}>
                          <strong>env secret:</strong> {gw.spec.envFromSecretRef}
                        </p>
                      )}
                      {allowedUsers.length > 0 && (
                        <p style={{ marginBottom: 4, fontSize: 13 }}>
                          <strong>allowed users:</strong>{' '}
                          {allowedUsers.slice(0, 3).map((u, i) => (
                            <Label key={`${u.channel ?? 'discord'}:${u.id}:${i}`} color="blue" style={{ marginRight: 4 }}>
                              {(u.channel ?? 'discord')}:{u.id.slice(-6)}
                            </Label>
                          ))}
                          {allowedUsers.length > 3 ? ` +${allowedUsers.length - 3}` : ''}
                        </p>
                      )}
                    </div>
                    <div
                      style={{
                        marginTop: 'auto',
                        paddingTop: 16,
                        display: 'flex',
                        flexDirection: 'column',
                        gap: 8,
                      }}
                    >
                      {gw.status?.gatewayEndpoint && (
                        <Button
                          component="a"
                          href={`${gw.status.gatewayEndpoint}/__openclaw__/canvas/`}
                          target="_blank"
                          variant="primary"
                          icon={<ExternalLinkAltIcon />}
                        >
                          Open Control UI
                        </Button>
                      )}
                    </div>
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

export default AgentGatewaysPage;
