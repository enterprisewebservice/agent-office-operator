import * as React from 'react';
import {
  K8sModel,
  K8sResourceCommon,
  k8sCreate,
  k8sDelete,
  k8sGet,
  k8sUpdate,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import { PageSection } from '@patternfly/react-core/dist/dynamic/components/Page';
import { Title } from '@patternfly/react-core/dist/dynamic/components/Title';
import { Card, CardBody, CardHeader, CardTitle } from '@patternfly/react-core/dist/dynamic/components/Card';
import { Label } from '@patternfly/react-core/dist/dynamic/components/Label';
import { Spinner } from '@patternfly/react-core/dist/dynamic/components/Spinner';
import { EmptyState, EmptyStateBody } from '@patternfly/react-core/dist/dynamic/components/EmptyState';
import { Button } from '@patternfly/react-core/dist/dynamic/components/Button';
import { Modal, ModalVariant } from '@patternfly/react-core/dist/dynamic/components/Modal';
import { Form, FormGroup, FormHelperText } from '@patternfly/react-core/dist/dynamic/components/Form';
import { TextInput } from '@patternfly/react-core/dist/dynamic/components/TextInput';
import { TextArea } from '@patternfly/react-core/dist/dynamic/components/TextArea';
import { FormSelect, FormSelectOption } from '@patternfly/react-core/dist/dynamic/components/FormSelect';
import { Radio } from '@patternfly/react-core/dist/dynamic/components/Radio';
import { Checkbox } from '@patternfly/react-core/dist/dynamic/components/Checkbox';
import { Alert } from '@patternfly/react-core/dist/dynamic/components/Alert';
import { HelperText, HelperTextItem } from '@patternfly/react-core/dist/dynamic/components/HelperText';
import { Bullseye } from '@patternfly/react-core/dist/dynamic/layouts/Bullseye';
import { Flex, FlexItem } from '@patternfly/react-core/dist/dynamic/layouts/Flex';
import { Gallery } from '@patternfly/react-core/dist/dynamic/layouts/Gallery';
import PlusCircleIcon from '@patternfly/react-icons/dist/dynamic/icons/plus-circle-icon';
import TrashIcon from '@patternfly/react-icons/dist/dynamic/icons/trash-icon';
import PencilAltIcon from '@patternfly/react-icons/dist/dynamic/icons/pencil-alt-icon';

/*
 * Model Connections — the admin side of "publish brains, don't hand
 * out keys". Full lifecycle in one page:
 *
 *   - publish (create) a connection with a friendly form,
 *   - the API key goes straight into a Secret in agent-office
 *     (write-only here — it is never read back or displayed),
 *   - visibility is groups/users the hiring UI filters on,
 *   - edit rotates the key only when a new one is typed,
 *   - delete unpublishes and (optionally) removes the key Secret.
 *
 * Everything runs with the console user's own credentials — someone
 * without RBAC on ModelConnections/Secrets gets the API's 403, not a
 * silent success.
 */

type ModelEntry = { id: string; name?: string };

type ModelConnection = K8sResourceCommon & {
  spec?: {
    displayName?: string;
    description?: string;
    kind?: 'subscription' | 'apiKey' | 'endpoint';
    provider?: string;
    baseUrl?: string;
    api?: string;
    apiKeySecretRef?: { name?: string; namespace?: string; key?: string };
    models?: ModelEntry[];
    keyStrategy?: string;
    access?: { groups?: string[]; users?: string[] };
  };
};

type SecretLite = K8sResourceCommon;

const ADMIN_NS = 'agent-office';

const ModelConnectionModel = {
  apiGroup: 'agentoffice.ai',
  apiVersion: 'v1alpha1',
  kind: 'ModelConnection',
  plural: 'modelconnections',
  abbr: 'MC',
  label: 'ModelConnection',
  labelPlural: 'ModelConnections',
  namespaced: false,
} as K8sModel;

const SecretModel = {
  apiVersion: 'v1',
  kind: 'Secret',
  plural: 'secrets',
  abbr: 'S',
  label: 'Secret',
  labelPlural: 'Secrets',
  namespaced: true,
} as K8sModel;

const kindColor = (k?: string) =>
  k === 'endpoint' ? 'blue' : k === 'subscription' ? 'purple' : 'gold';

const kindHelp: Record<string, string> = {
  endpoint:
    'Any OpenAI-compatible URL — MaaS, LiteLLM, vLLM. The operator renders it onto every gateway whose agents pick it and projects the key; agents never see credentials.',
  subscription:
    'A consumer-subscription route (ChatGPT/Codex). The gateway’s stored OAuth pays the bill; this entry just controls who may pick it.',
  apiKey:
    'The metered first-party API route (api.openai.com). Billing rides the gateway’s existing key; this entry controls who may pick it.',
};

const slugify = (s: string) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 63);

const splitList = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);

interface FormState {
  name: string;
  nameTouched: boolean;
  displayName: string;
  description: string;
  kind: 'endpoint' | 'subscription' | 'apiKey';
  provider: string;
  baseUrl: string;
  api: string;
  modelsText: string; // one per line: "id | label"
  groupsText: string;
  usersText: string;
  keyStrategy: string;
  apiKeyInput: string; // write-only
}

const emptyForm = (): FormState => ({
  name: '',
  nameTouched: false,
  displayName: '',
  description: '',
  kind: 'endpoint',
  provider: 'openai-codex',
  baseUrl: '',
  api: 'openai-completions',
  modelsText: '',
  groupsText: '',
  usersText: '',
  keyStrategy: 'shared',
  apiKeyInput: '',
});

const formFrom = (c: ModelConnection): FormState => ({
  name: c.metadata?.name ?? '',
  nameTouched: true,
  displayName: c.spec?.displayName ?? '',
  description: c.spec?.description ?? '',
  kind: (c.spec?.kind as FormState['kind']) ?? 'endpoint',
  provider: c.spec?.provider ?? 'openai-codex',
  baseUrl: c.spec?.baseUrl ?? '',
  api: c.spec?.api ?? 'openai-completions',
  modelsText: (c.spec?.models ?? [])
    .map((m) => (m.name && m.name !== m.id ? `${m.id} | ${m.name}` : m.id))
    .join('\n'),
  groupsText: (c.spec?.access?.groups ?? []).join(', '),
  usersText: (c.spec?.access?.users ?? []).join(', '),
  keyStrategy: c.spec?.keyStrategy ?? 'shared',
  apiKeyInput: '',
});

const parseModels = (text: string): ModelEntry[] =>
  text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const [id, ...rest] = l.split('|');
      const name = rest.join('|').trim();
      return name ? { id: id.trim(), name } : { id: id.trim() };
    });

const ModelConnectionsPage: React.FC = () => {
  const [conns, loaded, loadError] = useK8sWatchResource<ModelConnection[]>({
    groupVersionKind: { group: 'agentoffice.ai', version: 'v1alpha1', kind: 'ModelConnection' },
    isList: true,
  });
  // Key-Secret presence, so an endpoint connection whose Secret is
  // missing shows it loudly instead of failing agents quietly.
  const [secrets] = useK8sWatchResource<SecretLite[]>({
    groupVersionKind: { version: 'v1', kind: 'Secret' },
    namespace: ADMIN_NS,
    isList: true,
  });
  const secretNames = React.useMemo(
    () => new Set((secrets ?? []).map((s) => s.metadata?.name)),
    [secrets],
  );

  const [editing, setEditing] = React.useState<ModelConnection | 'new' | null>(null);
  const [form, setForm] = React.useState<FormState>(emptyForm());
  const [busy, setBusy] = React.useState(false);
  const [formError, setFormError] = React.useState<string | undefined>();
  const [deleting, setDeleting] = React.useState<ModelConnection | null>(null);
  const [deleteSecretToo, setDeleteSecretToo] = React.useState(true);

  const patch = (p: Partial<FormState>) => setForm((f) => ({ ...f, ...p }));

  const openCreate = () => {
    setForm(emptyForm());
    setFormError(undefined);
    setEditing('new');
  };
  const openEdit = (c: ModelConnection) => {
    setForm(formFrom(c));
    setFormError(undefined);
    setEditing(c);
  };

  const effectiveName = form.nameTouched || form.name ? form.name : slugify(form.displayName);

  // Where the key lives. On edit, respect whatever ref the connection
  // already carries (model-desk uses model-desk-agent-key); only fresh
  // connections get the managed `<name>-key` convention.
  const keyRef = (): { name: string; namespace: string; key: string } => {
    const existing = editing !== 'new' && editing ? editing.spec?.apiKeySecretRef : undefined;
    if (existing?.name) {
      return {
        name: existing.name,
        namespace: existing.namespace || ADMIN_NS,
        key: existing.key || 'api-key',
      };
    }
    return { name: `${effectiveName}-key`, namespace: ADMIN_NS, key: 'api-key' };
  };

  const validate = (): string | undefined => {
    if (!form.displayName.trim()) return 'Display name is required.';
    if (!effectiveName || !/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(effectiveName))
      return 'Name must be lowercase letters, digits and dashes.';
    if (form.kind === 'endpoint') {
      if (!/^https?:\/\//.test(form.baseUrl.trim()))
        return 'Endpoint connections need a http(s) base URL.';
      if (parseModels(form.modelsText).length === 0)
        return 'List at least one model (one per line, `id | label`).';
      const ref = keyRef();
      if (editing === 'new' && !form.apiKeyInput && !secretNames.has(ref.name))
        return `Enter an API key, or create Secret ${ADMIN_NS}/${ref.name} first.`;
    }
    if (!splitList(form.groupsText).length && !splitList(form.usersText).length)
      return 'Grant access to at least one group or user, or nobody will see it.';
    return undefined;
  };

  const save = async () => {
    const err = validate();
    if (err) {
      setFormError(err);
      return;
    }
    setBusy(true);
    setFormError(undefined);
    try {
      const ref = keyRef();
      // 1. Key first (endpoint kind, only when a new key was typed) —
      //    never store a connection that points at a key we failed to
      //    write.
      if (form.kind === 'endpoint' && form.apiKeyInput) {
        let existing: any;
        try {
          existing = await k8sGet({ model: SecretModel, name: ref.name, ns: ref.namespace });
        } catch (_e) {
          existing = undefined;
        }
        if (existing) {
          existing.stringData = { ...(existing.stringData ?? {}), [ref.key]: form.apiKeyInput };
          await k8sUpdate({ model: SecretModel, data: existing });
        } else {
          await k8sCreate({
            model: SecretModel,
            data: {
              apiVersion: 'v1',
              kind: 'Secret',
              metadata: { name: ref.name, namespace: ref.namespace },
              type: 'Opaque',
              stringData: { [ref.key]: form.apiKeyInput },
            } as any,
          });
        }
      }

      // 2. The connection itself.
      const spec: ModelConnection['spec'] = {
        displayName: form.displayName.trim(),
        ...(form.description.trim() ? { description: form.description.trim() } : {}),
        kind: form.kind,
        ...(form.kind !== 'endpoint' ? { provider: form.provider } : {}),
        ...(form.kind === 'endpoint'
          ? {
              baseUrl: form.baseUrl.trim(),
              api: form.api.trim() || 'openai-completions',
              apiKeySecretRef: ref,
            }
          : {}),
        models: parseModels(form.modelsText),
        keyStrategy: form.keyStrategy,
        access: {
          ...(splitList(form.groupsText).length ? { groups: splitList(form.groupsText) } : {}),
          ...(splitList(form.usersText).length ? { users: splitList(form.usersText) } : {}),
        },
      };

      if (editing === 'new') {
        await k8sCreate({
          model: ModelConnectionModel,
          data: {
            apiVersion: 'agentoffice.ai/v1alpha1',
            kind: 'ModelConnection',
            metadata: { name: effectiveName },
            spec,
          } as any,
        });
      } else if (editing) {
        const fresh: any = await k8sGet({
          model: ModelConnectionModel,
          name: editing.metadata!.name!,
        });
        fresh.spec = spec;
        await k8sUpdate({ model: ModelConnectionModel, data: fresh });
      }
      setEditing(null);
    } catch (e) {
      setFormError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const doDelete = async () => {
    if (!deleting) return;
    setBusy(true);
    try {
      const ref = deleting.spec?.apiKeySecretRef;
      await k8sDelete({ model: ModelConnectionModel, resource: deleting as any });
      if (
        deleteSecretToo &&
        deleting.spec?.kind === 'endpoint' &&
        ref?.name &&
        secretNames.has(ref.name)
      ) {
        await k8sDelete({
          model: SecretModel,
          resource: {
            metadata: { name: ref.name, namespace: ref.namespace || ADMIN_NS },
          } as any,
        });
      }
      setDeleting(null);
    } catch (e) {
      setFormError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      <PageSection variant="light">
        <Flex>
          <FlexItem>
            <Title headingLevel="h1">Model Connections</Title>
            <p style={{ marginTop: 8, color: 'var(--pf-v5-global--Color--200)' }}>
              The brains this platform publishes. Each connection is a model route
              with an access list — the hiring UI shows people only the
              connections you granted their group. Keys live in the{' '}
              <code>{ADMIN_NS}</code> namespace and are projected onto gateways by
              the operator; agents and attendees never see them.
            </p>
          </FlexItem>
          <FlexItem align={{ default: 'alignRight' }}>
            <Button variant="primary" icon={<PlusCircleIcon />} onClick={openCreate}>
              Publish a connection
            </Button>
          </FlexItem>
        </Flex>
      </PageSection>
      <PageSection>
        {!loaded && (
          <Bullseye>
            <Spinner />
          </Bullseye>
        )}
        {loadError && <p>Failed to load: {String(loadError)}</p>}
        {loaded && conns?.length === 0 && (
          <EmptyState>
            <EmptyStateBody>
              Nothing published yet. Publish a connection to put a brain on the
              hiring menu.
            </EmptyStateBody>
          </EmptyState>
        )}
        {loaded && conns && conns.length > 0 && (
          <Gallery hasGutter minWidths={{ default: '340px' }}>
            {conns.map((c) => {
              const ref = c.spec?.apiKeySecretRef;
              const isEndpoint = c.spec?.kind === 'endpoint';
              const keyOk = !isEndpoint || (ref?.name ? secretNames.has(ref.name) : false);
              const groups = c.spec?.access?.groups ?? [];
              const users = c.spec?.access?.users ?? [];
              return (
                <Card key={c.metadata?.uid} isCompact isFullHeight>
                  <CardHeader>
                    <CardTitle>
                      <Flex>
                        <FlexItem>
                          <strong>{c.spec?.displayName || c.metadata?.name}</strong>
                        </FlexItem>
                        <FlexItem align={{ default: 'alignRight' }}>
                          <Label color={kindColor(c.spec?.kind)}>{c.spec?.kind}</Label>
                        </FlexItem>
                      </Flex>
                    </CardTitle>
                  </CardHeader>
                  <CardBody>
                    {c.spec?.description && (
                      <p style={{ marginBottom: 8, color: 'var(--pf-v5-global--Color--200)' }}>
                        {c.spec.description}
                      </p>
                    )}
                    {isEndpoint && (
                      <p style={{ marginBottom: 4, fontSize: 13, wordBreak: 'break-all' }}>
                        <strong>endpoint:</strong> {c.spec?.baseUrl}
                      </p>
                    )}
                    {!isEndpoint && c.spec?.provider && (
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>provider:</strong> {c.spec.provider}
                      </p>
                    )}
                    <p style={{ marginBottom: 4, fontSize: 13 }}>
                      <strong>models:</strong>{' '}
                      {(c.spec?.models ?? []).map((m) => (
                        <Label key={m.id} isCompact style={{ marginRight: 4 }}>
                          {m.name || m.id}
                        </Label>
                      ))}
                    </p>
                    <p style={{ marginBottom: 4, fontSize: 13 }}>
                      <strong>visible to:</strong>{' '}
                      {groups.map((g) => (
                        <Label key={`g-${g}`} color="blue" isCompact style={{ marginRight: 4 }}>
                          group: {g}
                        </Label>
                      ))}
                      {users.map((u) => (
                        <Label key={`u-${u}`} color="purple" isCompact style={{ marginRight: 4 }}>
                          {u}
                        </Label>
                      ))}
                      {groups.length === 0 && users.length === 0 && (
                        <Label color="grey" isCompact>
                          nobody (hidden)
                        </Label>
                      )}
                    </p>
                    {isEndpoint && (
                      <p style={{ marginBottom: 4, fontSize: 13 }}>
                        <strong>key:</strong>{' '}
                        <Label color={keyOk ? 'green' : 'red'} isCompact>
                          {keyOk ? `stored (${ref?.name})` : ref?.name ? `missing: ${ref.name}` : 'none'}
                        </Label>
                      </p>
                    )}
                    <Flex style={{ marginTop: 12 }}>
                      <FlexItem>
                        <Button variant="secondary" icon={<PencilAltIcon />} onClick={() => openEdit(c)}>
                          Edit
                        </Button>
                      </FlexItem>
                      <FlexItem>
                        <Button
                          variant="danger"
                          icon={<TrashIcon />}
                          onClick={() => {
                            setDeleteSecretToo(true);
                            setDeleting(c);
                          }}
                        >
                          Unpublish
                        </Button>
                      </FlexItem>
                    </Flex>
                  </CardBody>
                </Card>
              );
            })}
          </Gallery>
        )}
      </PageSection>

      {/* Publish / edit */}
      <Modal
        variant={ModalVariant.medium}
        title={editing === 'new' ? 'Publish a connection' : `Edit ${form.displayName || form.name}`}
        isOpen={editing !== null}
        onClose={() => setEditing(null)}
        actions={[
          <Button key="save" variant="primary" onClick={save} isLoading={busy} isDisabled={busy}>
            {editing === 'new' ? 'Publish' : 'Save'}
          </Button>,
          <Button key="cancel" variant="link" onClick={() => setEditing(null)}>
            Cancel
          </Button>,
        ]}
      >
        <Form>
          {formError && <Alert variant="danger" isInline title={formError} />}
          <FormGroup label="What kind of route is this?" isRequired>
            {(['endpoint', 'subscription', 'apiKey'] as const).map((k) => (
              <Radio
                key={k}
                id={`kind-${k}`}
                name="kind"
                isChecked={form.kind === k}
                onChange={() => patch({ kind: k })}
                isDisabled={editing !== 'new'}
                label={
                  k === 'endpoint'
                    ? 'OpenAI-compatible endpoint'
                    : k === 'subscription'
                      ? 'Subscription (ChatGPT/Codex)'
                      : 'Metered API preset'
                }
                description={kindHelp[k]}
              />
            ))}
          </FormGroup>
          <FormGroup label="Display name" isRequired>
            <TextInput
              value={form.displayName}
              onChange={(_e, v) => patch({ displayName: v })}
              placeholder="Models-as-a-Service"
            />
          </FormGroup>
          <FormGroup label="Name">
            <TextInput
              value={effectiveName}
              onChange={(_e, v) => patch({ name: slugify(v), nameTouched: true })}
              isDisabled={editing !== 'new'}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  Becomes the provider id agents reference. Fixed after publish.
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Description">
            <TextInput
              value={form.description}
              onChange={(_e, v) => patch({ description: v })}
              placeholder="One line shown under the label on the picker"
            />
          </FormGroup>
          {form.kind !== 'endpoint' && (
            <FormGroup label="Provider preset" isRequired>
              <FormSelect value={form.provider} onChange={(_e, v) => patch({ provider: v })}>
                <FormSelectOption value="openai-codex" label="openai-codex (ChatGPT/Codex subscription)" />
                <FormSelectOption value="openai" label="openai (metered api.openai.com)" />
              </FormSelect>
            </FormGroup>
          )}
          {form.kind === 'endpoint' && (
            <>
              <FormGroup label="Base URL" isRequired>
                <TextInput
                  value={form.baseUrl}
                  onChange={(_e, v) => patch({ baseUrl: v })}
                  placeholder="http://model-desk.model-desk.svc.cluster.local:4000/v1"
                />
              </FormGroup>
              <FormGroup label="API dialect">
                <TextInput value={form.api} onChange={(_e, v) => patch({ api: v })} />
              </FormGroup>
              <FormGroup label="API key">
                <TextInput
                  type="password"
                  value={form.apiKeyInput}
                  onChange={(_e, v) => patch({ apiKeyInput: v })}
                  placeholder={
                    editing === 'new'
                      ? 'Stored as a Secret in agent-office; never shown again'
                      : `Leave blank to keep the current key (${keyRef().name})`
                  }
                />
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>
                      Write-only. Saved into Secret {ADMIN_NS}/{keyRef().name}; the operator
                      projects it onto gateways — nothing else can read it from here.
                    </HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FormGroup>
            </>
          )}
          <FormGroup label="Models" isRequired={form.kind === 'endpoint'}>
            <TextArea
              value={form.modelsText}
              onChange={(_e, v) => patch({ modelsText: v })}
              rows={3}
              placeholder={'gpt-4o-mini | GPT-4o mini\ngranite-3-3-8b | Granite 8B (sovereign)'}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  One per line: <code>id | label</code>. The first is the default.
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Visible to groups">
            <TextInput
              value={form.groupsText}
              onChange={(_e, v) => patch({ groupsText: v })}
              placeholder="attendees, developers"
            />
          </FormGroup>
          <FormGroup label="Visible to users">
            <TextInput
              value={form.usersText}
              onChange={(_e, v) => patch({ usersText: v })}
              placeholder="deanpeterson"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  Matched against the Developer Hub sign-in (group memberships and
                  username). Leave both empty and nobody sees it.
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FormGroup>
          <FormGroup label="Key strategy">
            <FormSelect value={form.keyStrategy} onChange={(_e, v) => patch({ keyStrategy: v })}>
              <FormSelectOption value="shared" label="shared — one key for every consumer" />
              <FormSelectOption
                value="perSeat"
                label="perSeat — per-consumer budgeted keys (minting lands with seat tooling)"
              />
            </FormSelect>
          </FormGroup>
        </Form>
      </Modal>

      {/* Unpublish */}
      <Modal
        variant={ModalVariant.small}
        title={`Unpublish ${deleting?.spec?.displayName || deleting?.metadata?.name}?`}
        isOpen={deleting !== null}
        onClose={() => setDeleting(null)}
        actions={[
          <Button key="del" variant="danger" onClick={doDelete} isLoading={busy} isDisabled={busy}>
            Unpublish
          </Button>,
          <Button key="cancel" variant="link" onClick={() => setDeleting(null)}>
            Cancel
          </Button>,
        ]}
      >
        <p>
          It disappears from every hiring menu, and the operator removes its
          provider blocks and projected keys from all gateways. Agents already
          pointed at it stop resolving this brain.
        </p>
        {deleting?.spec?.kind === 'endpoint' && deleting?.spec?.apiKeySecretRef?.name && (
          <Checkbox
            id="del-secret"
            style={{ marginTop: 12 }}
            isChecked={deleteSecretToo}
            onChange={(_e, v) => setDeleteSecretToo(v)}
            label={`Also delete key Secret ${ADMIN_NS}/${deleting.spec.apiKeySecretRef.name}`}
          />
        )}
      </Modal>
    </>
  );
};

export default ModelConnectionsPage;
