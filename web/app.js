const signInSection = document.querySelector('#sign-in');
const workspace = document.querySelector('#workspace');
const status = document.querySelector('#status');
const investigationSection = document.querySelector('#investigation');

const requestedInvestigation = (() => {
  const segments = window.location.pathname.split('/').filter(Boolean);
  if (segments[0] !== 'organizations' || segments[2] !== 'investigations' || !segments[3]) {
    return undefined;
  }
  try {
    return {
      organization: decodeURIComponent(segments[1]),
      investigation: decodeURIComponent(segments[3]),
      section: segments[4] || 'report',
    };
  } catch {
    return undefined;
  }
})();

let organization = requestedInvestigation?.organization || '';
let investigation = '';
let refreshTimer;

function explain(error) {
  status.textContent = error instanceof Error ? error.message : String(error);
}

async function request(url, { method = 'GET', body, authorization, organizationScoped = false } = {}) {
  const headers = {};
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (authorization) headers.Authorization = `Bearer ${authorization}`;
  if (organizationScoped) headers['X-OpenCluster-Organization'] = organization;

  const response = await fetch(url, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  let result;
  try {
    result = text ? JSON.parse(text) : {};
  } catch {
    result = { error: text };
  }
  if (!response.ok) throw new Error(result.error || `Request failed (${response.status})`);
  return result;
}

function formValues(form) {
  return Object.fromEntries(new FormData(form).entries());
}

async function restoreSession() {
  let session;
  try {
    session = await request('/api/v1/session', { organizationScoped: Boolean(organization) });
  } catch {
    signInSection.hidden = false;
    workspace.hidden = true;
    return;
  }

  const matchingMembership = session.organizations?.find(item => item.organizationId === organization);
  const membership = requestedInvestigation ? matchingMembership : matchingMembership || session.organizations?.[0];
  if (!membership) throw new Error('Your account does not belong to an organization.');
  organization = membership.organizationId;
  document.querySelector('#session-label').textContent =
    `${session.principal.displayName} · ${organization}`;
  signInSection.hidden = true;
  workspace.hidden = false;
  status.textContent = '';

  if (requestedInvestigation && !investigation) {
    investigation = requestedInvestigation.investigation;
    investigationSection.hidden = false;
    document.querySelector('#cancel').disabled = false;
    if (await refreshInvestigation()) {
      refreshTimer = setInterval(() => refreshInvestigation().catch(explain), 3000);
    }
    if (['report', 'hypotheses', 'activity', 'sources'].includes(requestedInvestigation.section)) {
      document.querySelector(`#${requestedInvestigation.section}`).scrollIntoView({ block: 'start' });
    }
  }
}

document.querySelector('#sign-in-form').addEventListener('submit', async event => {
  event.preventDefault();
  const values = formValues(event.currentTarget);
  organization = values.organization;
  try {
    await request('/api/v1/auth/local/sign-in', {
      method: 'POST',
      body: { organization, email: values.email, password: values.password },
    });
    event.currentTarget.reset();
    await restoreSession();
  } catch (error) {
    explain(error);
  }
});

document.querySelector('#oidc-sign-in').addEventListener('click', () => {
  const identifier = document.querySelector('#sign-in-form [name="organization"]').value.trim();
  if (!identifier) {
    explain('Enter your organization before continuing to your identity provider.');
    return;
  }
  const returnTo = window.location.pathname + window.location.search + window.location.hash;
  window.location.assign(`/api/v1/auth/oidc/start?organization=${encodeURIComponent(identifier)}&returnTo=${encodeURIComponent(returnTo)}`);
});

document.querySelector('#bootstrap-form').addEventListener('submit', async event => {
  event.preventDefault();
  const values = formValues(event.currentTarget);
  organization = values.organization;
  try {
    await request('/api/v1/auth/local/bootstrap', {
      method: 'POST',
      authorization: values.token,
      body: { organization, email: values.email, displayName: values.displayName, password: values.password },
    });
    event.currentTarget.reset();
    await restoreSession();
  } catch (error) {
    explain(error);
  }
});

async function refreshInvestigation() {
  const base = `/api/v1/investigations/${encodeURIComponent(investigation)}`;
  const [report, hypotheses, activity, sources] = await Promise.all([
    request(`${base}/report`, { organizationScoped: true }),
    request(`${base}/hypotheses`, { organizationScoped: true }),
    request(`${base}/activity`, { organizationScoped: true }),
    request(`${base}/sources`, { organizationScoped: true }),
  ]);
  for (const [name, value] of Object.entries({ report, hypotheses, activity, sources })) {
    document.querySelector(`#${name}`).textContent = JSON.stringify(value, null, 2);
  }
  if (['concluded', 'failed', 'cancelled'].includes(report.status)) {
    clearInterval(refreshTimer);
    document.querySelector('#cancel').disabled = true;
    return false;
  }
  return true;
}

document.querySelector('#investigation-form').addEventListener('submit', async event => {
  event.preventDefault();
  const values = formValues(event.currentTarget);
  try {
    const opened = await request('/api/v1/investigations', {
      method: 'POST',
      body: { question: values.question },
      organizationScoped: true,
    });
    investigation = opened.id || opened.investigation?.id;
    if (!investigation) {
      explain(opened.clarification || opened.question || 'Please clarify your investigation.');
      return;
    }
    investigationSection.hidden = false;
    document.querySelector('#cancel').disabled = false;
    clearInterval(refreshTimer);
    if (await refreshInvestigation()) {
      refreshTimer = setInterval(() => refreshInvestigation().catch(explain), 3000);
    }
  } catch (error) {
    explain(error);
  }
});

document.querySelector('#cancel').addEventListener('click', async () => {
  try {
    await request(`/api/v1/investigations/${encodeURIComponent(investigation)}/cancel`, {
      method: 'POST',
      organizationScoped: true,
    });
    await refreshInvestigation();
  } catch (error) {
    explain(error);
  }
});

document.querySelector('#sign-out').addEventListener('click', async () => {
  try {
    await request('/api/v1/session', { method: 'DELETE' });
    clearInterval(refreshTimer);
    investigationSection.hidden = true;
    await restoreSession();
  } catch (error) {
    explain(error);
  }
});

restoreSession().catch(explain);
