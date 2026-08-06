import { createContext, useContext, type PropsWithChildren } from 'react';
import { ResourceCatalogProvider } from '../components/ResourceCatalog';
import { apiClient, type ApiClient } from './client';

const ApiContext = createContext<ApiClient>(apiClient);

export function ApiProvider({ client = apiClient, children }: PropsWithChildren<{ client?: ApiClient }>) {
  return (
    <ApiContext.Provider value={client}>
      <ResourceCatalogProvider client={client}>{children}</ResourceCatalogProvider>
    </ApiContext.Provider>
  );
}

export function useApi(): ApiClient {
  return useContext(ApiContext);
}
