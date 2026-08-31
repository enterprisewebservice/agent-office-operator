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
import { Tooltip } from '@patternfly/react-core/dist/dynamic/components/Tooltip';
import { Bullseye } from '@patternfly/react-core/dist/dynamic/layouts/Bullseye';
import { Flex, FlexItem } from '@patternfly/react-core/dist/dynamic/layouts/Flex';
import { Gallery } from '@patternfly/react-core/dist/dynamic/layouts/Gallery';

/*
 * MaaS Grants — the who-sees-what matrix RHOAI 3.4 doesn't render.
 *
 * The dashboard's Models-as-a-Service list is silently filtered per
 * viewer and shows no access metadata; grants administration in 3.4
 * is YAML-only (MaaSSubscription = budgets, MaaSAuthPolicy = access,
 * and a model needs BOTH to answer). This page reads those CRs live
 * and renders, per model: which subscriptions budget it (with token
 * rate limits and owners) and which policies admit it (with
 * subjects). Read-only — the grants themselves live in git.
 */

const MAAS_NS = 'models-as-a-service';

type ModelRef = K8sResourceCommon & {
  status?: { phase?: string };
  spec?: { modelRef?: { kind?: string; name?: string }; endpointOverride?: string };
};

type Subscription = K8sResourceCommon & {
  spec?: {
    owner?: { groups?: { name: string }[]; users?: string[] };
    priority?: number;
    tokenMetadata?: { costCenter?: string };
    modelRefs?: {
      name: string;
      namespace: string;
      tokenRateLimits?: { limit: number; window: string }[];
    }[];
  };
};

type AuthPolicy = K8sResourceCommon & {
  spec?: {
    subjects?: { groups?: { name: string }[]; users?: string[] };
    modelRefs?: { name: string; namespace: string }[];
  };
};

const fmtLimit = (l: { limit: number; window: string }) =>
  `${l.limit >= 1000 ? `${l.limit / 1000}k` : l.limit}/${l.window}`;

const subjectChips = (
  who: { groups?: { name: string }[]; users?: string[] } | undefined,
  keyPrefix: string,
) => (
  <>
    {(who?.groups ?? []).map((g) => (
      <Label key={`${keyPrefix}-g-${g.name}`} color="blue" isCompact style={{ marginRight: 4 }}>
        group: {g.name}
      </Label>
    ))}
    {(who?.users ?? []).map((u) => (
      <Label key={`${keyPrefix}-u-${u}`} color="purple" isCompact style={{ marginRight: 4 }}>
        {u.startsWith('system:serviceaccount:') ? `sa: ${u.split(':').pop()}` : u}
      </Label>
    ))}
  </>
);

const MaaSGrantsPage: React.FC = () => {
  const [refs, refsLoaded, refsErr] = useK8sWatchResource<ModelRef[]>({
    groupVersionKind: { group: 'maas.opendatahub.io', version: 'v1alpha1', kind: 'MaaSModelRef' },
    namespace: MAAS_NS,
    isList: true,
  });
  const [subs] = useK8sWatchResource<Subscription[]>({
    groupVersionKind: { group: 'maas.opendatahub.io', version: 'v1alpha1', kind: 'MaaSSubscription' },
    namespace: MAAS_NS,
    isList: true,
  });
  const [pols] = useK8sWatchResource<AuthPolicy[]>({
    groupVersionKind: { group: 'maas.opendatahub.io', version: 'v1alpha1', kind: 'MaaSAuthPolicy' },
    namespace: MAAS_NS,
    isList: true,
  });

  const grantsFor = (model: string) => {
    const budget = (subs ?? [])
      .map((s) => {
        const ref = (s.spec?.modelRefs ?? []).find((m) => m.name === model);
        return ref ? { sub: s, limits: ref.tokenRateLimits ?? [] } : undefined;
      })
      .filter(Boolean) as { sub: Subscription; limits: { limit: number; window: string }[] }[];
    const access = (pols ?? []).filter((p) =>
      (p.spec?.modelRefs ?? []).some((m) => m.name === model),
    );
    return { budget, access };
  };

  return (
    <>
      <PageSection variant="light">
        <Title headingLevel="h1">MaaS Grants</Title>
        <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
          Who sees what on Models-as-a-Service. A model answers only for
          identities holding <strong>both</strong> a subscription (budget) and an
          authorization policy (access); the RHOAI dashboard filters its list
          per viewer without showing why. Grants are YAML in git — this page is
          the read-only matrix.
        </p>
      </PageSection>
      <PageSection>
        {!refsLoaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {refsErr && (
          <EmptyState>
            <EmptyStateBody>
              Could not read MaaS resources in {MAAS_NS}: {String(refsErr)}. This
              page needs cluster RBAC on maas.opendatahub.io (admins).
            </EmptyStateBody>
          </EmptyState>
        )}
        {refsLoaded && refs?.length === 0 && (
          <EmptyState>
            <EmptyStateBody>No MaaSModelRefs in {MAAS_NS} yet.</EmptyStateBody>
          </EmptyState>
        )}
        {refsLoaded && refs && refs.length > 0 && (
          <Gallery hasGutter minWidths={{ default: '360px' }}>
            {[...refs]
              .sort((a, b) => (a.metadata?.name ?? '').localeCompare(b.metadata?.name ?? ''))
              .map((r) => {
                const name = r.metadata?.name ?? '';
                const { budget, access } = grantsFor(name);
                const orphaned = budget.length === 0 || access.length === 0;
                return (
                  <Card key={r.metadata?.uid} isCompact isFullHeight>
                    <CardHeader>
                      <CardTitle>
                        <Flex>
                          <FlexItem>
                            <strong>{name}</strong>
                          </FlexItem>
                          <FlexItem align={{ default: 'alignRight' }}>
                            <Label
                              color={r.status?.phase === 'Ready' ? 'green' : 'orange'}
                              isCompact
                            >
                              {r.status?.phase ?? 'Unknown'}
                            </Label>
                          </FlexItem>
                        </Flex>
                      </CardTitle>
                    </CardHeader>
                    <CardBody>
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>budget via:</strong>{' '}
                        {budget.length === 0 && (
                          <Label color="red" isCompact>
                            no subscription
                          </Label>
                        )}
                        {budget.map(({ sub, limits }) => (
                          <Tooltip
                            key={`b-${sub.metadata?.name}`}
                            content={
                              <span>
                                owners: {(sub.spec?.owner?.groups ?? []).map((g) => g.name).join(', ') || '—'}
                                {(sub.spec?.owner?.users ?? []).length
                                  ? ` · users: ${(sub.spec?.owner?.users ?? []).join(', ')}`
                                  : ''}
                              </span>
                            }
                          >
                            <Label color="gold" isCompact style={{ marginRight: 4 }}>
                              {sub.metadata?.name} · {limits.map(fmtLimit).join(' + ') || 'no limit'}
                            </Label>
                          </Tooltip>
                        ))}
                      </p>
                      <div style={{ marginBottom: 4, fontSize: 13 }}>
                        {budget.map(({ sub }) => (
                          <p key={`bo-${sub.metadata?.name}`} style={{ margin: '2px 0 2px 12px' }}>
                            <span style={{ color: 'var(--pf-v5-global--Color--200)' }}>
                              {sub.metadata?.name}:
                            </span>{' '}
                            {subjectChips(
                              {
                                groups: sub.spec?.owner?.groups,
                                users: sub.spec?.owner?.users,
                              },
                              `sub-${sub.metadata?.name}`,
                            )}
                          </p>
                        ))}
                      </div>
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>access via:</strong>{' '}
                        {access.length === 0 && (
                          <Label color="red" isCompact>
                            no auth policy — always 403
                          </Label>
                        )}
                        {access.map((p) => (
                          <span key={`a-${p.metadata?.name}`}>
                            <Label color="cyan" isCompact style={{ marginRight: 4 }}>
                              {p.metadata?.name}
                            </Label>
                            {subjectChips(p.spec?.subjects, `pol-${p.metadata?.name}`)}
                          </span>
                        ))}
                      </p>
                      {orphaned && (
                        <p style={{ marginTop: 8, fontSize: 12, color: '#a15c07' }}>
                          Incomplete grant: a model needs a subscription AND an auth
                          policy to answer.
                        </p>
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

export default MaaSGrantsPage;
