import * as React from 'react';
import {
  K8sResourceCommon,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
// Console wraps plugin pages in its own <Page> — only PageSection here.
import { PageSection } from '@patternfly/react-core/dist/dynamic/components/Page';
import { Title } from '@patternfly/react-core/dist/dynamic/components/Title';
import { Card, CardBody, CardHeader, CardTitle } from '@patternfly/react-core/dist/dynamic/components/Card';
import { Label, LabelGroup } from '@patternfly/react-core/dist/dynamic/components/Label';
import { Alert } from '@patternfly/react-core/dist/dynamic/components/Alert';
import { Spinner } from '@patternfly/react-core/dist/dynamic/components/Spinner';
import { EmptyState, EmptyStateBody } from '@patternfly/react-core/dist/dynamic/components/EmptyState';
import { Button } from '@patternfly/react-core/dist/dynamic/components/Button';
import { Bullseye } from '@patternfly/react-core/dist/dynamic/layouts/Bullseye';
import { Flex, FlexItem } from '@patternfly/react-core/dist/dynamic/layouts/Flex';
import { Gallery } from '@patternfly/react-core/dist/dynamic/layouts/Gallery';
import ExternalLinkAltIcon from '@patternfly/react-icons/dist/dynamic/icons/external-link-alt-icon';
import UsersIcon from '@patternfly/react-icons/dist/dynamic/icons/users-icon';

type AgentWorkstation = K8sResourceCommon & {
  spec?: {
    displayName?: string;
    description?: string;
    emoji?: string;
    image?: string;
    team?: string;
    apiKeySecretRef?: string;
    model?: { provider?: string; modelName?: string };
    tools?: { allow?: string[] };
    memory?: { modules?: Array<{ name: string }> };
    runtime?: { shared?: { gatewayRef?: string } };
    channels?: {
      discord?: { url?: string };
    };
  };
  status?: {
    phase?: string;
    gatewayEndpoint?: string;
    message?: string;
    lastActivity?: string;
  };
};

const AW_GVK = { group: 'agentoffice.ai', version: 'v1alpha1', kind: 'AgentWorkstation' };
const PART_OF = 'app.kubernetes.io/part-of';

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

// Which team an agent belongs to, and how we know. spec.team is the
// declared answer; the part-of label is the convention crews already
// follow; namespace is the floor. Reporting the source matters — a team
// that exists only because two agents share a namespace is a guess, and
// the page should not present a guess as a roster.
type Grouping = { key: string; source: 'team' | 'label' | 'namespace' };

const teamOf = (a: AgentWorkstation): Grouping => {
  const declared = a.spec?.team?.trim();
  if (declared) return { key: declared, source: 'team' };
  const labelled = a.metadata?.labels?.[PART_OF]?.trim();
  if (labelled) return { key: labelled, source: 'label' };
  return { key: a.metadata?.namespace || 'default', source: 'namespace' };
};

const SOURCE_NOTE: Record<Grouping['source'], string> = {
  team: 'declared in spec.team',
  label: `inferred from the ${PART_OF} label`,
  namespace: 'no team set — grouped by namespace',
};

// Relative time, rounded the way a human reads a duty roster.
const relTime = (iso: string | undefined, now: number): string => {
  if (!iso) return 'no activity yet';
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return 'unknown';
  const secs = Math.max(0, Math.round((now - then) / 1000));
  if (secs < 60) return 'just now';
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
};

const activityColor = (iso: string | undefined, now: number) => {
  if (!iso) return 'grey';
  const age = now - Date.parse(iso);
  if (Number.isNaN(age)) return 'grey';
  if (age < 15 * 60 * 1000) return 'green';
  if (age < 2 * 60 * 60 * 1000) return 'blue';
  if (age < 24 * 60 * 60 * 1000) return 'gold';
  return 'grey';
};

const activityMs = (a: AgentWorkstation): number => {
  const t = a.status?.lastActivity;
  if (!t) return 0;
  const ms = Date.parse(t);
  return Number.isNaN(ms) ? 0 : ms;
};

const isForbidden = (err: unknown): boolean => {
  if (!err) return false;
  const anyErr = err as { response?: { status?: number }; code?: number; json?: { code?: number } };
  if (anyErr.response?.status === 403 || anyErr.code === 403 || anyErr.json?.code === 403) return true;
  return /forbidden|is not allowed|cannot list/i.test(String(err));
};

type Team = {
  key: string;
  source: Grouping['source'];
  agents: AgentWorkstation[];
  namespaces: string[];
  gateways: string[];
  lastActivity: number;
};

const buildTeams = (agents: AgentWorkstation[]): Team[] => {
  const byKey = new Map<string, Team>();
  for (const a of agents) {
    const { key, source } = teamOf(a);
    let t = byKey.get(key);
    if (!t) {
      t = { key, source, agents: [], namespaces: [], gateways: [], lastActivity: 0 };
      byKey.set(key, t);
    }
    // A declared team name outranks an inferred one when members disagree.
    if (source === 'team') t.source = 'team';
    else if (source === 'label' && t.source === 'namespace') t.source = 'label';

    t.agents.push(a);
    const ns = a.metadata?.namespace;
    if (ns && !t.namespaces.includes(ns)) t.namespaces.push(ns);
    const gw = a.spec?.runtime?.shared?.gatewayRef;
    if (gw && !t.gateways.includes(gw)) t.gateways.push(gw);
    t.lastActivity = Math.max(t.lastActivity, activityMs(a));
  }
  const teams = Array.from(byKey.values());
  for (const t of teams) {
    t.agents.sort((x, y) => activityMs(y) - activityMs(x));
    t.namespaces.sort();
    t.gateways.sort();
  }
  // Busiest crew first; a team that has never run sorts to the bottom.
  teams.sort((a, b) => b.lastActivity - a.lastActivity || a.key.localeCompare(b.key));
  return teams;
};

const AgentWorkstationsPage: React.FC = () => {
  // Watch every namespace: a team is only visible as a team if you can
  // see all of it, and the split-namespace check is meaningless scoped
  // to one namespace. Falls back to agent-office when the viewer lacks
  // cluster-scoped list rights, rather than showing an error page.
  const [allAgents, allLoaded, allError] = useK8sWatchResource<AgentWorkstation[]>({
    groupVersionKind: AW_GVK,
    isList: true,
  });
  const forbidden = isForbidden(allError);
  const [nsAgents, nsLoaded, nsError] = useK8sWatchResource<AgentWorkstation[]>(
    forbidden ? { groupVersionKind: AW_GVK, isList: true, namespace: 'agent-office' } : null,
  );

  const agents = forbidden ? nsAgents : allAgents;
  const loaded = forbidden ? nsLoaded : allLoaded;
  const loadError = forbidden ? nsError : allError;

  // Relative times go stale silently; re-render on a slow tick so
  // "2m ago" does not sit there reading "2m ago" an hour later.
  const [now, setNow] = React.useState<number>(() => Date.now());
  React.useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 30000);
    return () => window.clearInterval(id);
  }, []);

  const teams = React.useMemo(() => buildTeams(agents ?? []), [agents]);
  const split = teams.filter((t) => t.namespaces.length > 1);
  const totalAgents = agents?.length ?? 0;
  const running = (agents ?? []).filter((a) => a.status?.phase === 'Running').length;

  const devSpacesUrl = (name: string) =>
    `https://devspaces.apps.salamander.aimlworkbench.com/#https://github.com/enterprisewebservice/${name}-agent`;

  return (
    <>
      <PageSection variant="light">
        <Title headingLevel="h1">Agents &amp; Teams</Title>
        <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
          Every governed agent on this cluster, grouped into the team it works on. A team is one
          crew in one namespace — that namespace is its quota and blast-radius boundary — so a team
          whose members are scattered across namespaces is called out rather than quietly merged.
        </p>
        {loaded && totalAgents > 0 && (
          <Flex style={{ marginTop: 12 }} spaceItems={{ default: 'spaceItemsMd' }}>
            <FlexItem>
              <Label color="blue" icon={<UsersIcon />}>
                {teams.length} {teams.length === 1 ? 'team' : 'teams'}
              </Label>
            </FlexItem>
            <FlexItem>
              <Label color="green">
                {running}/{totalAgents} running
              </Label>
            </FlexItem>
            <FlexItem>
              <Label color="grey">
                {new Set((agents ?? []).map((a) => a.metadata?.namespace)).size} namespaces
              </Label>
            </FlexItem>
            {forbidden && (
              <FlexItem>
                <Label color="orange" title="You lack cluster-scoped list rights on AgentWorkstations">
                  scoped to agent-office
                </Label>
              </FlexItem>
            )}
          </Flex>
        )}
      </PageSection>
      <PageSection>
        {!loaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {loaded && loadError && (
          <Alert variant="danger" isInline title="Failed to load agents">
            {String(loadError)}
          </Alert>
        )}
        {loaded && !loadError && totalAgents === 0 && (
          <EmptyState>
            <EmptyStateBody>
              No AgentWorkstations on this cluster yet. Create one from the Developer Hub
              &ldquo;OpenClaw Agent&rdquo; template — describe the job and it composes the agent.
            </EmptyStateBody>
          </EmptyState>
        )}

        {loaded && split.length > 0 && (
          <Alert
            variant="warning"
            isInline
            title={`${split.length} ${split.length === 1 ? 'team is' : 'teams are'} split across namespaces`}
            style={{ marginBottom: 16 }}
          >
            {split.map((t) => (
              <div key={t.key}>
                <strong>{t.key}</strong> has members in {t.namespaces.join(', ')}. One team should
                live in one namespace so quota, NetworkPolicy, and RBAC apply to the whole crew at
                once. Either move the outliers, or set <code>spec.team</code> on them to say they
                are genuinely different teams.
              </div>
            ))}
          </Alert>
        )}

        {loaded &&
          teams.map((team) => (
            <div key={team.key} style={{ marginBottom: 32 }}>
              <Flex
                alignItems={{ default: 'alignItemsCenter' }}
                spaceItems={{ default: 'spaceItemsSm' }}
                style={{ marginBottom: 12 }}
              >
                <FlexItem>
                  <Title headingLevel="h2" size="xl">
                    <UsersIcon style={{ marginRight: 8, verticalAlign: 'baseline' }} />
                    {team.key}
                  </Title>
                </FlexItem>
                <FlexItem>
                  <LabelGroup>
                    <Label variant="outline" color="grey" title={SOURCE_NOTE[team.source]}>
                      {team.source === 'team'
                        ? 'spec.team'
                        : team.source === 'label'
                          ? 'part-of label'
                          : 'namespace-derived'}
                    </Label>
                    {team.namespaces.map((ns) => (
                      <Label
                        key={ns}
                        variant="outline"
                        color={team.namespaces.length > 1 ? 'orange' : 'blue'}
                      >
                        ns/{ns}
                      </Label>
                    ))}
                    {team.gateways.map((gw) => (
                      <Label key={gw} variant="outline" color="cyan">
                        gw/{gw}
                      </Label>
                    ))}
                  </LabelGroup>
                </FlexItem>
                <FlexItem align={{ default: 'alignRight' }}>
                  <span style={{ color: 'var(--pf-v5-global--Color--200)', fontSize: 13 }}>
                    {team.agents.length} {team.agents.length === 1 ? 'agent' : 'agents'} · last
                    active{' '}
                    {team.lastActivity
                      ? relTime(new Date(team.lastActivity).toISOString(), now)
                      : 'never'}
                  </span>
                </FlexItem>
              </Flex>

              <Gallery hasGutter>
                {team.agents.map((a) => {
                  const memoryRefs = a.spec?.memory?.modules ?? [];
                  const tools = a.spec?.tools?.allow ?? [];
                  const last = a.status?.lastActivity;
                  return (
                    // isFullHeight + flex-column body so the action row
                    // pushes to the bottom regardless of content length —
                    // every card's buttons line up at the same Y.
                    <Card
                      key={a.metadata?.uid}
                      isCompact
                      isFullHeight
                      style={{ display: 'flex', flexDirection: 'column' }}
                    >
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
                      <CardBody style={{ display: 'flex', flexDirection: 'column', flex: 1 }}>
                        <div>
                          {a.spec?.description && (
                            <p style={{ marginBottom: 8, color: 'var(--pf-v5-global--Color--200)' }}>
                              {a.spec.description}
                            </p>
                          )}
                          <p style={{ marginBottom: 8 }}>
                            <Label
                              isCompact
                              color={activityColor(last, now)}
                              title={last ? `last activity ${last}` : 'this agent has not processed a request yet'}
                            >
                              {relTime(last, now)}
                            </Label>
                          </p>
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
                        </div>
                        {/* Bottom action row pinned to card bottom by
                            marginTop:auto. Buttons stack vertically with
                            `gap: 8` so they don't touch. */}
                        <div
                          style={{
                            marginTop: 'auto',
                            paddingTop: 16,
                            display: 'flex',
                            flexDirection: 'column',
                            gap: 8,
                          }}
                        >
                          {a.spec?.channels?.discord?.url && (
                            <Button
                              component="a"
                              href={a.spec.channels.discord.url}
                              target="_blank"
                              variant="primary"
                              icon={<ExternalLinkAltIcon />}
                            >
                              Open in Discord
                            </Button>
                          )}
                          <Button
                            component="a"
                            href={devSpacesUrl(a.metadata?.name ?? '')}
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
            </div>
          ))}
      </PageSection>
    </>
  );
};

export default AgentWorkstationsPage;
