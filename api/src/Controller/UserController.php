<?php

namespace App\Controller;

use App\Service\UserService;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\JsonResponse;
use Symfony\Component\HttpFoundation\Request;
use Symfony\Component\HttpFoundation\Response;
use Symfony\Component\Routing\Attribute\Route;
use Symfony\Component\Security\Http\Attribute\IsGranted;

#[Route('/api/users')]
#[IsGranted('ROLE_SYSOP')]
class UserController extends AbstractController
{
    public function __construct(
        private UserService $userService
    ) {}

    #[Route('', name: 'api_users_list', methods: ['GET'])]
    public function list(Request $request): JsonResponse
    {
        $page = max(1, $request->query->getInt('page', 1));
        $limit = min(100, max(1, $request->query->getInt('limit', 25)));
        $search = $request->query->get('search');

        $result = $this->userService->listUsers($page, $limit, $search);

        return $this->json([
            'users' => $result['users'],
            'total' => $result['total'],
            'page' => $page,
            'limit' => $limit,
        ]);
    }

    #[Route('/{id}', name: 'api_users_get', methods: ['GET'])]
    public function get(string $id): JsonResponse
    {
        $user = $this->userService->getUser($id);
        if ($user === null) {
            return $this->json(['error' => 'User not found.'], Response::HTTP_NOT_FOUND);
        }

        return $this->json($user);
    }

    #[Route('/{id}', name: 'api_users_update', methods: ['PATCH'])]
    public function update(string $id, Request $request): JsonResponse
    {
        $data = json_decode($request->getContent(), true);
        $user = $this->userService->updateUser($id, $data);

        if ($user === null) {
            return $this->json(['error' => 'User not found.'], Response::HTTP_NOT_FOUND);
        }

        return $this->json($user);
    }

    #[Route('/{id}', name: 'api_users_delete', methods: ['DELETE'])]
    public function delete(string $id): JsonResponse
    {
        $deleted = $this->userService->deleteUser($id);
        if (!$deleted) {
            return $this->json(['error' => 'User not found.'], Response::HTTP_NOT_FOUND);
        }

        return $this->json(null, Response::HTTP_NO_CONTENT);
    }
}
